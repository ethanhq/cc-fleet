package workflow

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// --- test harness: a deterministic fake leaf via the runLeaf seam ----------------

type leafCall struct {
	vendor, prompt, runID, phase, label, model string
	timeout                                    time.Duration
	maxBudget                                  float64
	maxTurns                                   int
}

type recorder struct {
	mu    sync.Mutex
	calls []leafCall
}

func (r *recorder) record(c leafCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

func (r *recorder) snapshot() []leafCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]leafCall(nil), r.calls...)
}

func (r *recorder) prompts() []string {
	out := []string{}
	for _, c := range r.snapshot() {
		out = append(out, c.prompt)
	}
	return out
}

// fakeLeaf adapts a per-call responder into a runLeaf, recording every request and
// stamping the run/phase/label/vendor back onto the Result (as subagent.Run does).
func fakeLeaf(r *recorder, respond func(leafCall) subagent.Result) func(subagent.Request) subagent.Result {
	return func(req subagent.Request) subagent.Result {
		prompt := ""
		if req.PromptReader != nil {
			b, _ := io.ReadAll(req.PromptReader)
			prompt = string(b)
		}
		c := leafCall{
			vendor: req.Vendor, prompt: prompt, runID: req.RunID, phase: req.Phase, label: req.Label,
			model: req.Model, timeout: req.Timeout, maxBudget: req.MaxBudgetUSD, maxTurns: req.MaxTurns,
		}
		r.record(c)
		res := respond(c)
		res.RunID, res.Phase, res.Label, res.Vendor = req.RunID, req.Phase, req.Label, req.Vendor
		return res
	}
}

// echoLeaf returns OK with "ok:<prompt>".
func echoLeaf(r *recorder) func(subagent.Request) subagent.Result {
	return fakeLeaf(r, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
}

// runScript runs src with a fake leaf and returns the script's module globals. It
// isolates ConfigDir to a temp dir so any manifest writes stay out of the real home.
func runScript(t *testing.T, runID string, concurrency int, leaf func(subagent.Request) subagent.Result, src string) (starlark.StringDict, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	eng := &engine{sched: newScheduler(context.Background(), concurrency), runID: runID}
	return eng.run("test.star", src, Options{})
}

// --- small starlark-value assertion helpers --------------------------------------

func wantStringList(t *testing.T, g starlark.StringDict, name string) []starlark.Value {
	t.Helper()
	v, ok := g[name]
	if !ok {
		t.Fatalf("global %q not set", name)
	}
	l, ok := v.(*starlark.List)
	if !ok {
		t.Fatalf("global %q is %s, want list", name, v.Type())
	}
	out := make([]starlark.Value, 0, l.Len())
	for i := 0; i < l.Len(); i++ {
		out = append(out, l.Index(i))
	}
	return out
}

func asStr(t *testing.T, v starlark.Value) string {
	t.Helper()
	s, ok := starlark.AsString(v)
	if !ok {
		t.Fatalf("value %v is %s, want string", v, v.Type())
	}
	return s
}

// --- tests -----------------------------------------------------------------------

func TestParallelFanout(t *testing.T) {
	rec := &recorder{}
	g, err := runScript(t, "run1", 4, echoLeaf(rec), `
results = parallel([
    lambda: agent("a", vendor="v"),
    lambda: agent("b", vendor="v"),
    lambda: agent("c", vendor="v"),
])
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := wantStringList(t, g, "results")
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	// parallel preserves index order even though execution is concurrent.
	for i, want := range []string{"ok:a", "ok:b", "ok:c"} {
		if s := asStr(t, got[i]); s != want {
			t.Errorf("results[%d] = %q, want %q", i, s, want)
		}
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("leaf called %d times, want 3", n)
	}
}

func TestPipelineChaining(t *testing.T) {
	rec := &recorder{}
	// stage2 sees stage1's output as `prev`; assert the chain by echoing it forward.
	g, err := runScript(t, "run2", 4, echoLeaf(rec), `
results = pipeline(
    ["x", "y"],
    lambda prev, item, i: agent("s1:" + item, vendor="v"),
    lambda prev, item, i: agent("s2:" + prev, vendor="v"),
)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := wantStringList(t, g, "results")
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	// item "x": s1 → "ok:s1:x", then s2 prompt "s2:ok:s1:x" → "ok:s2:ok:s1:x".
	if s := asStr(t, got[0]); s != "ok:s2:ok:s1:x" {
		t.Errorf("results[0] = %q, want chained ok:s2:ok:s1:x", s)
	}
	if s := asStr(t, got[1]); s != "ok:s2:ok:s1:y" {
		t.Errorf("results[1] = %q", s)
	}
}

func TestForBreakLoopUntilDry(t *testing.T) {
	rec := &recorder{}
	// A bounded loop that breaks once it has accumulated 2 — the faithful "loop until
	// dry" idiom (no `while`).
	g, err := runScript(t, "run3", 2, echoLeaf(rec), `
found = []
for _ in range(10):
    r = agent("probe", vendor="v")
    found.append(r)
    if len(found) >= 2:
        break
n = len(found)
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	n, ok := g["n"]
	if !ok {
		t.Fatal("n not set")
	}
	if i, _ := starlark.AsInt32(n); i != 2 {
		t.Errorf("loop ran to n=%v, want 2 (break)", n)
	}
	if c := len(rec.prompts()); c != 2 {
		t.Errorf("leaf called %d times, want 2 (loop broke early)", c)
	}
}

func TestAgentFailureRaisesAtTopLevel(t *testing.T) {
	rec := &recorder{}
	failLeaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "KEY_INVALID", ErrorMsg: "bad key"}
	})
	_, err := runScript(t, "run4", 2, failLeaf, `x = agent("go", vendor="v")`)
	if err == nil {
		t.Fatal("expected a top-level agent failure to abort the run, got nil error")
	}
	if !strings.Contains(err.Error(), "KEY_INVALID") {
		t.Errorf("error %q should carry the leaf error code", err.Error())
	}
}

func TestParallelCatchesFailureAsNone(t *testing.T) {
	rec := &recorder{}
	// First prompt fails, second succeeds → [None, "ok:b"].
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "a" {
			return subagent.Result{OK: false, ErrorCode: "RATE_LIMITED", ErrorMsg: "slow down"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	g, err := runScript(t, "run5", 4, leaf, `
results = parallel([lambda: agent("a", vendor="v"), lambda: agent("b", vendor="v")])
a_is_none = results[0] == None
b = results[1]
`)
	if err != nil {
		t.Fatalf("run: %v (a failing branch must NOT abort parallel)", err)
	}
	if v := g["a_is_none"]; v != starlark.Bool(true) {
		t.Errorf("failed branch should be None, got a_is_none=%v", v)
	}
	if s := asStr(t, g["b"]); s != "ok:b" {
		t.Errorf("surviving branch = %q, want ok:b", s)
	}
}

func TestSchemaRetryThenValid(t *testing.T) {
	rec := &recorder{}
	var n int
	var mu sync.Mutex
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt == 1 {
			return subagent.Result{OK: true, Result: "not json at all"}
		}
		return subagent.Result{OK: true, Result: `{"answer": 42}`}
	})
	g, err := runScript(t, "run6", 1, leaf, `
res = agent("compute", vendor="v", schema={"required": ["answer"]})
ans = res["answer"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["ans"]); i != 42 {
		t.Errorf("ans = %v, want 42 (retry should have recovered)", g["ans"])
	}
	if n < 2 {
		t.Errorf("expected at least one retry, leaf ran %d times", n)
	}
}

func TestSchemaMissingKeyRetry(t *testing.T) {
	rec := &recorder{}
	var n int
	var mu sync.Mutex
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt == 1 {
			return subagent.Result{OK: true, Result: `{"other": 1}`} // valid JSON, missing required key
		}
		return subagent.Result{OK: true, Result: `{"answer": 7}`}
	})
	g, err := runScript(t, "run7", 1, leaf, `
res = agent("q", vendor="v", schema={"required": ["answer"]})
ans = res["answer"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["ans"]); i != 7 {
		t.Errorf("ans = %v, want 7", g["ans"])
	}
}

// TestSharedMutableFrozenError is the convert-under-lock completion (review C1): a
// def-built thunk list that mutates a shared captured container must NOT race the
// detector — freezing each thunk before release turns the mutation into a
// deterministic Starlark error, surfaced as None. Running this under `-race` is the
// real assertion (a clean race detector); the None results confirm the frozen path.
func TestSharedMutableFrozenError(t *testing.T) {
	rec := &recorder{}
	g, err := runScript(t, "run8", 4, echoLeaf(rec), `
def build():
    acc = []
    thunks = []
    for p in ["a", "b", "c", "d"]:
        thunks.append(lambda: acc.append(agent(p, vendor="v")))
    return thunks

results = parallel(build())
all_none = all([r == None for r in results])
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if v := g["all_none"]; v != starlark.Bool(true) {
		t.Errorf("thunks mutating a shared frozen list should each fail to None; got %v", g["results"])
	}
}

// TestLeafPanicRecovered (review C4): a panicking leaf inside a parallel thunk is
// recovered to None — the run survives, the process does not crash.
func TestLeafPanicRecovered(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "boom" {
			panic("leaf exploded")
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	g, err := runScript(t, "run9", 4, leaf, `
results = parallel([lambda: agent("boom", vendor="v"), lambda: agent("fine", vendor="v")])
zero_none = results[0] == None
one = results[1]
`)
	if err != nil {
		t.Fatalf("run: %v (a panicking leaf must not abort the run)", err)
	}
	if g["zero_none"] != starlark.Bool(true) {
		t.Errorf("panicking branch should be None")
	}
	if s := asStr(t, g["one"]); s != "ok:fine" {
		t.Errorf("surviving branch = %q", s)
	}
}

// TestCancelSkipsQueuedLeaves (review C4/S4): cancelling the engine ctx makes queued
// agents return promptly (None) while an in-flight leaf is joined before the barrier
// returns — no hang, no abandoned goroutine (wg.Wait is the join).
func TestCancelSkipsQueuedLeaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		started <- struct{}{}
		<-release
		return subagent.Result{OK: true, Result: "ok"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	ctx, cancel := context.WithCancel(context.Background())
	eng := &engine{sched: newScheduler(ctx, 1), runID: "run10"} // pool of 1 → 2 queued
	src := `results = parallel([lambda: agent("0", vendor="v"), lambda: agent("1", vendor="v"), lambda: agent("2", vendor="v")])
n_none = len([r for r in results if r == None])`

	done := make(chan starlark.StringDict, 1)
	go func() {
		g, err := eng.run("c.star", src, Options{})
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- g
	}()

	<-started // one leaf is in-flight (holds the only slot); the other two are queued
	cancel()  // queued agents' acquireSlot returns false → None
	// The in-flight leaf keeps the single slot, so the two queued goroutines' select
	// has only ctx.Done ready (slots full) → they deterministically resolve to None.
	// Give them a beat to do so BEFORE freeing the slot, so a freed slot can't be
	// grabbed instead (cancel is best-effort; this just makes the test deterministic).
	time.Sleep(100 * time.Millisecond)
	close(release) // now let the in-flight leaf finish and the barrier complete

	select {
	case g := <-done:
		if i, _ := starlark.AsInt32(g["n_none"]); i != 2 {
			t.Errorf("n_none = %v, want 2 (two queued leaves cancelled)", g["n_none"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel — possible deadlock/leak")
	}
}

func TestLeafTagging(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "RID", 4, echoLeaf(rec), `
phase("plan")
x = agent("p1", vendor="deepseek", label="planner")
y = agent("p2", vendor="glm", phase="explicit", label="other")
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	calls := rec.snapshot()
	byPrompt := map[string]leafCall{}
	for _, c := range calls {
		byPrompt[c.prompt] = c
	}
	if c := byPrompt["p1"]; c.runID != "RID" || c.phase != "plan" || c.label != "planner" || c.vendor != "deepseek" {
		t.Errorf("p1 tagged %+v, want runID=RID phase=plan label=planner vendor=deepseek", c)
	}
	if c := byPrompt["p2"]; c.phase != "explicit" || c.vendor != "glm" {
		t.Errorf("p2 tagged %+v, want phase=explicit (explicit phase= overrides current) vendor=glm", c)
	}
}

func TestAgentRequiresVendor(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "run11", 2, echoLeaf(rec), `x = agent("hi")`)
	if err == nil || !strings.Contains(err.Error(), "vendor") {
		t.Errorf("expected a vendor-required error, got %v", err)
	}
}

// TestAgentOptionalNoneAccepted: passing an explicit None for every optional (the
// documented "omitted" default) must behave like omitting them, not error.
func TestAgentOptionalNoneAccepted(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "rn", 2, echoLeaf(rec),
		`x = agent("p", vendor="v", model=None, schema=None, label=None, phase=None, timeout=None, max_budget_usd=None, max_turns=None)`)
	if err != nil {
		t.Fatalf("explicit None for optionals must be accepted: %v", err)
	}
	c := rec.snapshot()[0]
	if c.model != "" || c.label != "" || c.phase != "" || c.timeout != 0 || c.maxBudget != 0 || c.maxTurns != 0 {
		t.Errorf("None optionals should map to zero values, got %+v", c)
	}
}

// TestAgentParamPlumbing asserts model/timeout/max_turns/max_budget_usd reach the
// Request, and that an INT timeout + INT budget are accepted (the strict-typing fix).
func TestAgentParamPlumbing(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "rp", 2, echoLeaf(rec),
		`x = agent("p", vendor="v", model="m", timeout=42, max_turns=3, max_budget_usd=2)`)
	if err != nil {
		t.Fatalf("run: %v (int timeout/budget must be accepted)", err)
	}
	c := rec.snapshot()[0]
	if c.model != "m" {
		t.Errorf("model = %q, want m", c.model)
	}
	if c.timeout != 42*time.Second {
		t.Errorf("timeout = %v, want 42s", c.timeout)
	}
	if c.maxTurns != 3 {
		t.Errorf("maxTurns = %d, want 3", c.maxTurns)
	}
	if c.maxBudget != 2 {
		t.Errorf("maxBudget = %v, want 2", c.maxBudget)
	}
}

// TestPipelineStageFailureToNone: a stage that fails drops its item to None and skips
// the item's remaining stages (the asymmetric, less-obvious pipeline path).
func TestPipelineStageFailureToNone(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if strings.HasPrefix(c.prompt, "s1:bad") {
			return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "stage1 fail"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	g, err := runScript(t, "rpf", 4, leaf, `
results = pipeline(
    ["good", "bad"],
    lambda prev, item, i: agent("s1:" + item, vendor="v"),
    lambda prev, item, i: agent("s2:" + prev, vendor="v"),
)
bad_is_none = results[1] == None
good = results[0]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if g["bad_is_none"] != starlark.Bool(true) {
		t.Errorf("the item whose stage1 failed should be None")
	}
	if s := asStr(t, g["good"]); s != "ok:s2:ok:s1:good" {
		t.Errorf("good = %q, want chained", s)
	}
	stage2 := 0
	for _, p := range rec.prompts() {
		if strings.HasPrefix(p, "s2:") {
			stage2++
		}
	}
	if stage2 != 1 {
		t.Errorf("stage2 ran %d times, want 1 (skipped for the failed item)", stage2)
	}
}

// TestSchemaPropertiesKeys: with no `required`, the schema's `properties` keys are
// enforced (the v1 shallow heuristic).
func TestSchemaPropertiesKeys(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `{"a": 1, "b": 2}`}
	})
	g, err := runScript(t, "rsp", 1, leaf, `
res = agent("q", vendor="v", schema={"properties": {"a": {}, "b": {}}})
both = res["a"] + res["b"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["both"]); i != 3 {
		t.Errorf("both = %v, want 3", g["both"])
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"  {\"a\":1}  ", `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"```json\n{\"a\":1}", `{"a":1}`}, // unclosed fence: drop the opener, keep the body
	}
	for _, c := range cases {
		if got := stripCodeFence(c.in); got != c.want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestArgsPredeclared(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	eng := &engine{sched: newScheduler(context.Background(), 2), runID: "ra"}
	g, err := eng.run("a.star", `n = args["count"]`, Options{ArgsJSON: `{"count": 7}`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["n"]); i != 7 {
		t.Errorf("args[count] = %v, want 7", g["n"])
	}
}

// TestConcurrentPhaseLog (review C3): phase()/log() driven from concurrent parallel
// goroutines must be GIL-serialized — the manifest read-modify-write in AppendRunPhase
// must not lose updates. Run under -race; assert all 8 distinct phases landed.
func TestConcurrentPhaseLog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "c.star")
	src := `meta = {"name": "n", "description": "d"}
def w(i):
    phase("p%d" % i)
    log("at %d" % i)
    return agent("t%d" % i, vendor="v")
results = parallel([lambda: w(0), lambda: w(1), lambda: w(2), lambda: w(3),
                    lambda: w(4), lambda: w(5), lambda: w(6), lambda: w(7)])
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, _ := subagent.ReadRun(run.RunID)
	if len(got.Phases) != 8 {
		t.Errorf("manifest has %d phases, want 8 (no lost update under concurrent AppendRunPhase)", len(got.Phases))
	}
}

// TestExecuteFinalizesFailedStatus: a top-level agent failure makes Execute return the
// error AND flip the manifest to failed (the "a detached run always finalizes" guard).
func TestExecuteFinalizesFailedStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = func(subagent.Request) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "KEY_INVALID", ErrorMsg: "nope"}
	}
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "f.star")
	os.WriteFile(script, []byte("meta = {\"name\": \"n\", \"description\": \"d\"}\nx = agent(\"go\", vendor=\"v\")\n"), 0o600)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err == nil {
		t.Fatal("expected Execute to surface the leaf failure")
	}
	got, _ := subagent.ReadRun(run.RunID)
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if !strings.Contains(got.Error, "KEY_INVALID") {
		t.Errorf("run.Error = %q, want the failure cause persisted", got.Error)
	}
}

// TestExecuteRejectsBadRunID: a path-unsafe run id is refused before the script runs.
func TestExecuteRejectsBadRunID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	script := filepath.Join(dir, "s.star")
	if err := os.WriteFile(script, []byte(`meta = {"name": "n", "description": "d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, "../evil", Options{}); err == nil {
		t.Error("Execute must reject a path-unsafe run id")
	}
}

// TestExecutePanicFinalizesFailed: a panicking leaf on the top-level thread is recovered
// by Execute into status=failed + a wrapped error — the process never crashes.
func TestExecutePanicFinalizesFailed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = func(subagent.Request) subagent.Result { panic("boom") }
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "p.star")
	os.WriteFile(script, []byte("meta = {\"name\": \"n\", \"description\": \"d\"}\nx = agent(\"go\", vendor=\"v\")\n"), 0o600)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	err = Execute(context.Background(), script, run.RunID, Options{})
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("expected a recovered panic error, got %v", err)
	}
	got, _ := subagent.ReadRun(run.RunID)
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
}
