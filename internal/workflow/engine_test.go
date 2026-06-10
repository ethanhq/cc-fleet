package workflow

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// --- test harness: a deterministic fake leaf via the runLeaf seam ----------------

type leafCall struct {
	vendor, prompt, runID, phase, label, model string
	timeout                                    time.Duration
	maxBudget                                  float64
	maxTurns                                   int
	persistIO                                  bool
	ioPrompt                                   string
	workingDir                                 string
	promptProfile                              string
	tools                                      []string
	noSkills                                   bool
	mcp                                        bool
	jsonSchema                                 string
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
func fakeLeaf(r *recorder, respond func(leafCall) subagent.Result) func(context.Context, subagent.Request) subagent.Result {
	return func(_ context.Context, req subagent.Request) subagent.Result {
		prompt := ""
		if req.PromptReader != nil {
			b, _ := io.ReadAll(req.PromptReader)
			prompt = string(b)
		}
		c := leafCall{
			vendor: req.Vendor, prompt: prompt, runID: req.RunID, phase: req.Phase, label: req.Label,
			model: req.Model, timeout: req.Timeout, maxBudget: req.MaxBudgetUSD, maxTurns: req.MaxTurns,
			persistIO: req.PersistIO, ioPrompt: req.IOPrompt, workingDir: req.WorkingDir,
			promptProfile: req.PromptProfile, tools: req.Tools, noSkills: req.NoSkills, mcp: req.MCP,
			jsonSchema: req.JSONSchema,
		}
		r.record(c)
		res := respond(c)
		res.RunID, res.Phase, res.Label, res.Vendor = req.RunID, req.Phase, req.Label, req.Vendor
		return res
	}
}

// echoLeaf returns OK with "ok:<prompt>".
func echoLeaf(r *recorder) func(context.Context, subagent.Request) subagent.Result {
	return fakeLeaf(r, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
}

// newTestEngine wires an engine the way Execute does (run/leaf ctx pair + scheduler),
// minus the manifest/journal/events plumbing the fake-leaf unit tests don't need.
func newTestEngine(ctx context.Context, runID string, concurrency int) *engine {
	leafCtx, cancel := context.WithCancel(ctx)
	return &engine{
		sched: newScheduler(leafCtx, concurrency), runID: runID,
		runCtx: ctx, leafCtx: leafCtx, cancelLeaves: cancel,
	}
}

// runScript runs src with a fake leaf and returns the script's settled value (its
// top-level `return`). It isolates ConfigDir to a temp dir so any manifest writes stay
// out of the real home, and pins resolveProfile to identity so the default-slim leaf
// shape (and any journal key) never depends on the host claude version.
func runScript(t *testing.T, runID string, concurrency int, leaf func(context.Context, subagent.Request) subagent.Result, src string) (goja.Value, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	eng := newTestEngine(context.Background(), runID, concurrency)
	return eng.run("test.js", []byte(src), Options{})
}

// --- exported-value assertion helpers ---------------------------------------------

// wantMap exports the script's returned value as an object.
func wantMap(t *testing.T, v goja.Value) map[string]interface{} {
	t.Helper()
	if v == nil {
		t.Fatal("script returned no value")
	}
	m, ok := v.Export().(map[string]interface{})
	if !ok {
		t.Fatalf("script returned %T, want an object", v.Export())
	}
	return m
}

func listField(t *testing.T, m map[string]interface{}, name string) []interface{} {
	t.Helper()
	v, ok := m[name]
	if !ok {
		t.Fatalf("field %q not returned", name)
	}
	l, ok := v.([]interface{})
	if !ok {
		t.Fatalf("field %q is %T, want array", name, v)
	}
	return l
}

func strAt(t *testing.T, lst []interface{}, i int) string {
	t.Helper()
	s, ok := lst[i].(string)
	if !ok {
		t.Fatalf("element %d is %T (%v), want string", i, lst[i], lst[i])
	}
	return s
}

func intField(t *testing.T, m map[string]interface{}, name string) int64 {
	t.Helper()
	switch n := m[name].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	t.Fatalf("field %q is %T (%v), want number", name, m[name], m[name])
	return 0
}

func strField(t *testing.T, m map[string]interface{}, name string) string {
	t.Helper()
	s, ok := m[name].(string)
	if !ok {
		t.Fatalf("field %q is %T (%v), want string", name, m[name], m[name])
	}
	return s
}

// --- tests -----------------------------------------------------------------------

func TestParallelFanout(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "run1", 4, echoLeaf(rec), `
const results = await parallel([
    () => agent("a", {vendor: "v"}),
    () => agent("b", {vendor: "v"}),
    () => agent("c", {vendor: "v"}),
]);
return { results };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := listField(t, wantMap(t, v), "results")
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}
	// parallel preserves index order even though execution is concurrent.
	for i, want := range []string{"ok:a", "ok:b", "ok:c"} {
		if s := strAt(t, got, i); s != want {
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
	v, err := runScript(t, "run2", 4, echoLeaf(rec), `
const results = await pipeline(
    ["x", "y"],
    (prev, item, i) => agent("s1:" + item, {vendor: "v"}),
    (prev, item, i) => agent("s2:" + prev, {vendor: "v"}),
);
return { results };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := listField(t, wantMap(t, v), "results")
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	// item "x": s1 → "ok:s1:x", then s2 prompt "s2:ok:s1:x" → "ok:s2:ok:s1:x".
	if s := strAt(t, got, 0); s != "ok:s2:ok:s1:x" {
		t.Errorf("results[0] = %q, want chained ok:s2:ok:s1:x", s)
	}
	if s := strAt(t, got, 1); s != "ok:s2:ok:s1:y" {
		t.Errorf("results[1] = %q", s)
	}
}

func TestLoopUntilDry(t *testing.T) {
	rec := &recorder{}
	// A plain while loop that breaks once it has accumulated 2 — the loop-until-dry
	// idiom, now in its native form.
	v, err := runScript(t, "run3", 2, echoLeaf(rec), `
const found = [];
while (found.length < 2) {
    found.push(await agent("probe", {vendor: "v"}));
}
return { n: found.length };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "n"); n != 2 {
		t.Errorf("loop ran to n=%v, want 2", n)
	}
	if c := len(rec.prompts()); c != 2 {
		t.Errorf("leaf called %d times, want 2 (loop broke early)", c)
	}
}

func TestAgentFailureRejectsAtTopLevel(t *testing.T) {
	rec := &recorder{}
	failLeaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "KEY_INVALID", ErrorMsg: "bad key"}
	})
	_, err := runScript(t, "run4", 2, failLeaf, `return await agent("go", {vendor: "v"});`)
	if err == nil {
		t.Fatal("expected a top-level agent failure to abort the run, got nil error")
	}
	if !strings.Contains(err.Error(), "KEY_INVALID") {
		t.Errorf("error %q should carry the leaf error code", err.Error())
	}
}

func TestParallelCatchesFailureAsNull(t *testing.T) {
	rec := &recorder{}
	// First prompt fails, second succeeds → [null, "ok:b"].
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "a" {
			return subagent.Result{OK: false, ErrorCode: "RATE_LIMITED", ErrorMsg: "slow down"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	v, err := runScript(t, "run5", 4, leaf, `
const results = await parallel([() => agent("a", {vendor: "v"}), () => agent("b", {vendor: "v"})]);
return { aIsNull: results[0] === null, b: results[1] };
`)
	if err != nil {
		t.Fatalf("run: %v (a failing branch must NOT abort parallel)", err)
	}
	m := wantMap(t, v)
	if m["aIsNull"] != true {
		t.Errorf("failed branch should be null, got aIsNull=%v", m["aIsNull"])
	}
	if s := strField(t, m, "b"); s != "ok:b" {
		t.Errorf("surviving branch = %q, want ok:b", s)
	}
}

// TestSchemaStructuredOutputReturned: a schema leaf passes the schema through the
// request (claude enforces the StructuredOutput call) — the prompt stays BARE, the
// validated payload flows back into the script, and the leaf runs exactly once.
func TestSchemaStructuredOutputReturned(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "prose", StructuredOutput: json.RawMessage(`{"answer": 42}`)}
	})
	v, err := runScript(t, "run6", 1, leaf, `
const res = await agent("compute", {vendor: "v", schema: {required: ["answer"]}});
return { ans: res.answer };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "ans"); n != 42 {
		t.Errorf("ans = %v, want 42 (the structured payload)", n)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("leaf ran %d times, want 1", len(calls))
	}
	if c := calls[0]; c.prompt != "compute" {
		t.Errorf("prompt = %q, want the bare prompt (no injected schema instruction)", c.prompt)
	}
	if c := calls[0]; c.jsonSchema != `{"required":["answer"]}` {
		t.Errorf("JSONSchema = %q, want the canonical schema JSON", c.jsonSchema)
	}
}

// TestSchemaNotSatisfiedTerminal: a structured payload that fails validation is
// TERMINAL — the run aborts after exactly one exec (an identical re-run would only
// reproduce the failure at full leaf cost).
func TestSchemaNotSatisfiedTerminal(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"other": 1}`)}
	})
	_, err := runScript(t, "run7", 1, leaf, `
return await agent("q", {vendor: "v", schema: {required: ["answer"]}});
`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("err = %v, want a schema-not-satisfied failure", err)
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("leaf ran %d times, want exactly 1 (no retry)", n)
	}
}

// TestSharedStateDeterministic: thunks mutating a shared captured array are LEGAL on
// the single-threaded loop (where Starlark needed freeze-to-error) — every push lands,
// deterministically, and -race stays clean because only the loop runs JS.
func TestSharedStateDeterministic(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "run8", 4, echoLeaf(rec), `
const acc = [];
await parallel(["a", "b", "c", "d"].map((p) => async () => { acc.push(await agent(p, {vendor: "v"})); }));
return { n: acc.length };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "n"); n != 4 {
		t.Errorf("shared array got %d pushes, want 4", n)
	}
}

// TestLeafPanicRecovered: a panicking leaf inside a parallel thunk is recovered to a
// rejection (→ null) — the run survives, the process does not crash.
func TestLeafPanicRecovered(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "boom" {
			panic("leaf exploded")
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	v, err := runScript(t, "run9", 4, leaf, `
const results = await parallel([() => agent("boom", {vendor: "v"}), () => agent("fine", {vendor: "v"})]);
return { zeroNull: results[0] === null, one: results[1] };
`)
	if err != nil {
		t.Fatalf("run: %v (a panicking leaf must not abort the run)", err)
	}
	m := wantMap(t, v)
	if m["zeroNull"] != true {
		t.Errorf("panicking branch should be null")
	}
	if s := strField(t, m, "one"); s != "ok:fine" {
		t.Errorf("surviving branch = %q", s)
	}
}

// TestCancelStopsRun: cancelling the run ctx mid-flight stops the run — queued leaves
// never launch, the in-flight leaf's exec ctx cancels, the loop drains, and the run
// returns a stop (not a hang, not a leak) with eng.stopped set for the "stopped"
// manifest finalize.
func TestCancelStopsRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	started := make(chan struct{}, 1)
	leaf := func(ctx context.Context, req subagent.Request) subagent.Result {
		started <- struct{}{}
		<-ctx.Done() // the leaf dies when its exec ctx cancels
		return subagent.Result{OK: false, ErrorCode: "SUBAGENT_STOPPED", ErrorMsg: "stopped"}
	}
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	ctx, cancel := context.WithCancel(context.Background())
	eng := newTestEngine(ctx, "run10", 1) // pool of 1 → 2 queued behind the in-flight leaf
	src := `return await parallel([() => agent("0", {vendor: "v"}), () => agent("1", {vendor: "v"}), () => agent("2", {vendor: "v"})]);`

	done := make(chan error, 1)
	go func() {
		_, err := eng.run("c.js", []byte(src), Options{})
		done <- err
	}()

	<-started // one leaf is in-flight (holds the only slot); the other two are queued
	cancel()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Errorf("err = %v, want a run-stopped error", err)
		}
		if !eng.stopped {
			t.Error("eng.stopped not set — the manifest would finalize failed, not stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel — possible deadlock/leak")
	}
}

func TestLeafTagging(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "RID", 4, echoLeaf(rec), `
phase("plan");
await agent("p1", {vendor: "deepseek", label: "planner"});
await agent("p2", {vendor: "glm", phase: "explicit", label: "other"});
return {};
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
		t.Errorf("p2 tagged %+v, want phase=explicit (explicit phase overrides current) vendor=glm", c)
	}
}

func TestAgentRequiresVendor(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "run11", 2, echoLeaf(rec), `return await agent("hi");`)
	if err == nil || !strings.Contains(err.Error(), "vendor") {
		t.Errorf("expected a vendor-required error, got %v", err)
	}
}

// TestAgentUnknownOptionRejected: a typo'd option must fail loudly, not silently no-op
// (the weak-model footgun this runtime exists to remove).
func TestAgentUnknownOptionRejected(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "run12", 2, echoLeaf(rec), `return await agent("hi", {vendor: "v", modle: "x"});`)
	if err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("expected an unknown-option error, got %v", err)
	}
}

// TestAgentOptionalNullAccepted: passing an explicit null for every optional (the
// documented "omitted" default) must behave like omitting them, not error.
func TestAgentOptionalNullAccepted(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "rn", 2, echoLeaf(rec),
		`return await agent("p", {vendor: "v", model: null, schema: null, label: null, phase: null, timeout: null, max_budget_usd: null, max_turns: null});`)
	if err != nil {
		t.Fatalf("explicit null for optionals must be accepted: %v", err)
	}
	c := rec.snapshot()[0]
	if c.model != "" || c.label != "" || c.phase != "" || c.timeout != 0 || c.maxBudget != 0 || c.maxTurns != 0 {
		t.Errorf("null optionals should map to zero values, got %+v", c)
	}
}

// TestAgentParamPlumbing asserts model/timeout/max_turns/max_budget_usd reach the
// Request, and that integer numbers are accepted for the float options.
func TestAgentParamPlumbing(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "rp", 2, echoLeaf(rec),
		`return await agent("p", {vendor: "v", model: "m", timeout: 42, max_turns: 3, max_budget_usd: 2});`)
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

// TestPipelineStageFailureToNull: a stage that fails drops its item to null and skips
// the item's remaining stages (the asymmetric, less-obvious pipeline path).
func TestPipelineStageFailureToNull(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if strings.HasPrefix(c.prompt, "s1:bad") {
			return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "stage1 fail"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	v, err := runScript(t, "rpf", 4, leaf, `
const results = await pipeline(
    ["good", "bad"],
    (prev, item, i) => agent("s1:" + item, {vendor: "v"}),
    (prev, item, i) => agent("s2:" + prev, {vendor: "v"}),
);
return { badIsNull: results[1] === null, good: results[0] };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if m["badIsNull"] != true {
		t.Errorf("the item whose stage1 failed should be null")
	}
	if s := strField(t, m, "good"); s != "ok:s2:ok:s1:good" {
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
// enforced on present values (JSON-Schema semantics).
func TestSchemaPropertiesKeys(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a": 1, "b": 2}`)}
	})
	v, err := runScript(t, "rsp", 1, leaf, `
const res = await agent("q", {vendor: "v", schema: {properties: {a: {}, b: {}}}});
return { both: res.a + res.b };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "both"); n != 3 {
		t.Errorf("both = %v, want 3", n)
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

func TestArgsValue(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	eng := newTestEngine(context.Background(), "ra", 2)
	v, err := eng.run("a.js", []byte(`return { n: args.count };`), Options{ArgsJSON: `{"count": 7}`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "n"); n != 7 {
		t.Errorf("args.count = %v, want 7", n)
	}
}

// TestUnawaitedLeafCompletes: a leaf the script never awaits still runs to completion
// before the run finalizes (the in-flight counter drains the loop), and a fulfilled
// unawaited promise is not an "unhandled rejection".
func TestUnawaitedLeafCompletes(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "rb", 2, echoLeaf(rec), `
agent("background", {vendor: "v"});
return { fg: await agent("foreground", {vendor: "v"}) };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := strField(t, wantMap(t, v), "fg"); s != "ok:foreground" {
		t.Errorf("fg = %q", s)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("leaf ran %d times, want 2 (the unawaited leaf still completed)", n)
	}
}

// TestUnhandledRejectionFailsRun: a leaf that failed with NOBODY ever handling its
// rejection fails the otherwise-successful run — a silently dropped failure is still
// a failure.
func TestUnhandledRejectionFailsRun(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "bad" {
			return subagent.Result{OK: false, ErrorCode: "KEY_INVALID", ErrorMsg: "nope"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	_, err := runScript(t, "rur", 2, leaf, `
agent("bad", {vendor: "v"});
return await agent("good", {vendor: "v"});
`)
	if err == nil || !strings.Contains(err.Error(), "unhandled promise rejection") {
		t.Fatalf("err = %v, want an unhandled-rejection failure", err)
	}
}

// TestDelayedAwaitIsHandled: a leaf promise that rejects BEFORE the script attaches a
// handler is not a false failure — awaiting it later (in a try/catch) clears it from
// the unhandled set (handled-set semantics).
func TestDelayedAwaitIsHandled(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "bad" {
			return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "fail fast"}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	v, err := runScript(t, "rda", 2, leaf, `
const p = agent("bad", {vendor: "v"});
await agent("good", {vendor: "v"}); // the bad leaf rejects while this awaits
try {
    await p;
    return { caught: false };
} catch (e) {
    return { caught: true };
}
`)
	if err != nil {
		t.Fatalf("run: %v (a later-handled rejection must not fail the run)", err)
	}
	if m := wantMap(t, v); m["caught"] != true {
		t.Errorf("caught = %v, want true", m["caught"])
	}
}

// TestNoProgressDetection: a script awaiting a promise nothing can ever settle fails
// explicitly instead of hanging the engine forever.
func TestNoProgressDetection(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "rnp", 2, echoLeaf(rec), `await new Promise(() => {}); return {};`)
	if err == nil || !strings.Contains(err.Error(), "nothing will ever settle") {
		t.Fatalf("err = %v, want the no-progress failure", err)
	}
}

// TestDeterminismLockdown: the nondeterministic surface throws (or is absent) BEFORE
// any user statement could observe it — Date / Math.random / eval / Function / the
// prototype-constructor escape / timers / console / require.
func TestDeterminismLockdown(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "rdl", 1, echoLeaf(rec), `
const threw = (f) => { try { f(); return false; } catch (e) { return true; } };
return {
    dateThrows: threw(() => Date.now()),
    newDateThrows: threw(() => new Date()),
    randomThrows: threw(() => Math.random()),
    noEval: typeof eval === "undefined",
    noFunction: typeof Function === "undefined",
    ctorSealed: threw(() => (function () {}).constructor("return 1")()),
    asyncCtorSealed: threw(() => (async function () {}).constructor("return 1")),
    noSetTimeout: typeof setTimeout === "undefined",
    noConsole: typeof console === "undefined",
    noRequire: typeof require === "undefined",
};
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	for _, k := range []string{"dateThrows", "newDateThrows", "randomThrows", "noEval", "noFunction", "ctorSealed", "asyncCtorSealed", "noSetTimeout", "noConsole", "noRequire"} {
		if m[k] != true {
			t.Errorf("%s = %v, want true", k, m[k])
		}
	}
}

// TestAgentNonFiniteNumberRejected: Infinity/NaN numerics would corrupt the budget
// arithmetic (Inf reserved − Inf released = NaN) — rejected at the option boundary.
func TestAgentNonFiniteNumberRejected(t *testing.T) {
	rec := &recorder{}
	for _, src := range []string{
		`return await agent("p", {vendor: "v", timeout: Infinity});`,
		`return await agent("p", {vendor: "v", max_budget_usd: 0/0});`,
	} {
		_, err := runScript(t, "rnf", 2, echoLeaf(rec), src)
		if err == nil || !strings.Contains(err.Error(), "finite") {
			t.Errorf("err = %v, want a finite-number rejection for %q", err, src)
		}
	}
}

// TestDateDeterministicEntryPoints: explicit-value Date construction, parse, and UTC
// stay usable (they are pure functions of their inputs); only the wall-clock entry
// points throw.
func TestDateDeterministicEntryPoints(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "rdd", 1, echoLeaf(rec), `
const threw = (f) => { try { f(); return false; } catch (e) { return true; } };
return {
    iso: new Date(0).toISOString(),
    parsed: Date.parse("1970-01-02T00:00:00.000Z"),
    utc: Date.UTC(1970, 0, 2),
    nowThrows: threw(() => Date.now()),
    arglessThrows: threw(() => new Date()),
};
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if s := strField(t, m, "iso"); s != "1970-01-01T00:00:00.000Z" {
		t.Errorf("iso = %q", s)
	}
	if n := intField(t, m, "parsed"); n != 86400000 {
		t.Errorf("parsed = %v, want 86400000", n)
	}
	if n := intField(t, m, "utc"); n != 86400000 {
		t.Errorf("utc = %v, want 86400000", n)
	}
	if m["nowThrows"] != true || m["arglessThrows"] != true {
		t.Errorf("wall-clock entry points must throw: %v", m)
	}
}

// TestWorkflowArgsCapabilityFree: child args cross as a JSON clone, so a parent can't
// smuggle its real `workflow` function past the one-level guard.
func TestWorkflowArgsCapabilityFree(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	child := filepath.Join(dir, "child.js")
	if err := os.WriteFile(child, []byte(`const meta = {name: "c", description: "d"};
return { wfType: typeof args.wf, n: args.n };
`), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `return await workflow(` + strconv.Quote(child) + `, {wf: workflow, n: 1});`
	v, err := runScript(t, "rcf", 2, echoLeaf(rec), src)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if s := strField(t, m, "wfType"); s != "undefined" {
		t.Errorf("args.wf crossed the child boundary as %q, want undefined (JSON clone)", s)
	}
	if n := intField(t, m, "n"); n != 1 {
		t.Errorf("plain data should survive the clone, n = %v", n)
	}
}

// TestSchemaCanonicalJSSemantics: schema canonicalization follows JS JSON semantics —
// an undefined-valued member drops — and the Go re-encode sorts keys byte-stably.
func TestSchemaCanonicalJSSemantics(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a": 1}`)}
	})
	_, err := runScript(t, "rcs", 1, leaf, `
return await agent("q", {vendor: "v", schema: {required: ["a"], junk: undefined}});
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c := rec.snapshot()[0]; c.jsonSchema != `{"required":["a"]}` {
		t.Errorf("JSONSchema = %q, want the undefined member dropped", c.jsonSchema)
	}
}

// TestConcurrentPhaseLog: phase()/log() driven from concurrent parallel thunks are
// loop-serialized — every distinct phase lands on the manifest, none lost.
func TestConcurrentPhaseLog(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "c.js")
	src := `const meta = {name: "n", description: "d"};
const w = async (i) => {
    phase("p" + i);
    log("at " + i);
    return agent("t" + i, {vendor: "v"});
};
await parallel([0, 1, 2, 3, 4, 5, 6, 7].map((i) => () => w(i)));
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
		t.Errorf("manifest has %d phases, want 8 (no lost update)", len(got.Phases))
	}
}

// TestExecuteFinalizesFailedStatus: a top-level agent failure makes Execute return the
// error AND flip the manifest to failed (the "a detached run always finalizes" guard).
func TestExecuteFinalizesFailedStatus(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = func(context.Context, subagent.Request) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "KEY_INVALID", ErrorMsg: "nope"}
	}
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "f.js")
	os.WriteFile(script, []byte("const meta = {name: \"n\", description: \"d\"};\nreturn await agent(\"go\", {vendor: \"v\"});\n"), 0o600)
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

// TestExecuteStoppedStatusOnCancel: cancelling the run ctx finalizes the manifest
// "stopped" — never "failed" — for the cooperative signal path.
func TestExecuteStoppedStatusOnCancel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	started := make(chan struct{}, 1)
	old := runLeaf
	runLeaf = func(ctx context.Context, req subagent.Request) subagent.Result {
		started <- struct{}{}
		<-ctx.Done()
		return subagent.Result{OK: false, ErrorCode: "SUBAGENT_STOPPED", ErrorMsg: "stopped"}
	}
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "s.js")
	os.WriteFile(script, []byte("const meta = {name: \"n\", description: \"d\"};\nreturn await agent(\"go\", {vendor: \"v\"});\n"), 0o600)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Execute(ctx, script, run.RunID, Options{}) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after cancel")
	}
	got, _ := subagent.ReadRun(run.RunID)
	if got.Status != "stopped" {
		t.Errorf("status = %q, want stopped", got.Status)
	}
}

// TestExecuteRejectsBadRunID: a path-unsafe run id is refused before the script runs.
func TestExecuteRejectsBadRunID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	script := filepath.Join(dir, "s.js")
	if err := os.WriteFile(script, []byte(`const meta = {name: "n", description: "d"};`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, "../evil", Options{}); err == nil {
		t.Error("Execute must reject a path-unsafe run id")
	}
}

// TestExecutePanicFinalizesFailed: a panicking leaf is recovered into a rejection and
// the run finalizes failed — the process never crashes.
func TestExecutePanicFinalizesFailed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = func(context.Context, subagent.Request) subagent.Result { panic("boom") }
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "p.js")
	os.WriteFile(script, []byte("const meta = {name: \"n\", description: \"d\"};\nreturn await agent(\"go\", {vendor: \"v\"});\n"), 0o600)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	err = Execute(context.Background(), script, run.RunID, Options{})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("expected a recovered leaf panic error, got %v", err)
	}
	got, _ := subagent.ReadRun(run.RunID)
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// TestExportConstMetaAccepted: the native `export const meta` prefix runs verbatim
// (the offset-preserving strip), and Prepare reads the meta through it.
func TestExportConstMetaAccepted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	script := filepath.Join(dir, "e.js")
	src := "export const meta = {name: \"native\", description: \"d\"};\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare must accept `export const meta`: %v", err)
	}
	if run.Name != "native" {
		t.Errorf("run.Name = %q, want native", run.Name)
	}
}

// TestESModulesRejectedExplicitly: real module syntax gets the explicit unsupported
// error, not a bare parse failure.
func TestESModulesRejectedExplicitly(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	script := filepath.Join(dir, "m.js")
	src := "import fs from \"fs\";\nconst meta = {name: \"n\", description: \"d\"};\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(script)
	if err == nil || !strings.Contains(err.Error(), "ES modules") {
		t.Fatalf("err = %v, want the explicit ES-modules error", err)
	}
}
