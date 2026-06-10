package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// gateLeaf is a controllable fake leaf: each prompt blocks until its gate channel is
// closed OR its exec ctx cancels (returning the stopped class, like the real Run).
type gateLeaf struct {
	mu    sync.Mutex
	gates map[string]chan struct{}
	rec   *recorder
}

func newGateLeaf(rec *recorder) *gateLeaf {
	return &gateLeaf{gates: map[string]chan struct{}{}, rec: rec}
}

func (g *gateLeaf) gate(prompt string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.gates[prompt]
	if !ok {
		ch = make(chan struct{})
		g.gates[prompt] = ch
	}
	return ch
}

func (g *gateLeaf) release(prompt string) { close(g.gate(prompt)) }

func (g *gateLeaf) run(ctx context.Context, req subagent.Request) subagent.Result {
	return fakeLeaf(g.rec, func(c leafCall) subagent.Result {
		stopped := subagent.Result{OK: false, ErrorCode: subagent.ErrCodeStopped, ErrorMsg: "killed"}
		select {
		case <-g.gate(c.prompt):
			// Cancellation wins when it lands in the SAME window as the gate close: a
			// killed leaf returns stopped, it does not "succeed" because the gate opened
			// (a real leaf's process is dead). Without this, Go's random ready-case pick
			// makes the stop/restart sequence flaky on a slow scheduler.
			select {
			case <-ctx.Done():
				return stopped
			default:
				return subagent.Result{OK: true, Result: fmt.Sprintf("ok:%s#%d", c.prompt, c.attempt)}
			}
		case <-ctx.Done():
			return stopped
		}
	})(ctx, req)
}

// ctlHarness runs src on a controllable engine and returns the engine plus a done
// channel carrying the script's settled value.
func ctlHarness(t *testing.T, runID string, concurrency int, leaf func(context.Context, subagent.Request) subagent.Result, src string) (*engine, chan goja.Value, chan error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	eng := newTestEngine(context.Background(), runID, concurrency)
	vals, errs := make(chan goja.Value, 1), make(chan error, 1)
	go func() {
		v, err := eng.run("c.js", []byte(src), Options{})
		vals <- v
		errs <- err
	}()
	return eng, vals, errs
}

// jobByLabel polls the jobs dir until the labeled leaf reaches one of the wanted
// statuses, returning its Result.
func jobByLabel(t *testing.T, label string, want ...string) subagent.Result {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := subagent.ListJobs()
		for _, j := range jobs {
			if j.Label != label {
				continue
			}
			for _, w := range want {
				if j.Status == w {
					return j
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("leaf %q never reached %v", label, want)
	return subagent.Result{}
}

// waitCalled polls until the fake leaf for prompt has been invoked (its exec started —
// the meta-status stays "queued" under a fake leaf, which never registers a real PID).
func waitCalled(t *testing.T, rec *recorder, prompt string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range rec.prompts() {
			if p == prompt {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("leaf %q never started executing", prompt)
}

// directive posts a control command onto the loop the way the poller does.
func directive(eng *engine, op, leafID string) {
	eng.post(leafCB{state: func() { eng.applyDirective(ctlCommand{Op: op, Leaf: leafID}) }})
}

// TestLeafStopHoldsAndRestartResumes (W-stop-leaf + W-restart-held): stopping one arm
// of a live parallel holds exactly that leaf — the sibling completes undisturbed and
// the run cannot finalize — and restarting it re-runs the SAME job in place
// (Attempt 2), after which the run completes with both values.
func TestLeafStopHoldsAndRestartResumes(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	eng, vals, errs := ctlHarness(t, "wsl", 2, g.run, `
return await parallel([
    () => agent("a", {vendor: "v", label: "leaf-a"}),
    () => agent("b", {vendor: "v", label: "leaf-b"}),
]);`)

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	directive(eng, "stop", a.JobID)
	if held := jobByLabel(t, "leaf-a", "held"); held.Status != "held" {
		t.Fatalf("leaf-a = %q, want held", held.Status)
	}
	// The sibling is untouched: release it; it finishes while a stays held (its value
	// shows up in the final results below) and the run still cannot finalize.
	g.release("b")
	select {
	case err := <-errs:
		t.Fatalf("run finalized while a leaf was held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// Wake the held leaf; its second attempt completes the run.
	g.release("a")
	directive(eng, "restart", a.JobID)
	v := <-vals
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.Export().([]interface{})
	if got[0] != "ok:a#2" || got[1] != "ok:b#1" {
		t.Fatalf("results = %v, want [ok:a#2 ok:b#1]", got)
	}
	if res := subagent.StatusFor(a.JobID); res.Attempt != 2 {
		t.Errorf("leaf-a final attempt = %d, want 2 (a fake leaf writes no terminal cache; the value above is the done signal)", res.Attempt)
	}
}

// TestLeafRestartInflight (W-restart-inflight): restarting a RUNNING leaf kills the
// attempt and re-execs in place; the script receives attempt 2's answer and the
// journal holds exactly one entry for the key.
func TestLeafRestartInflight(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	eng, vals, errs := ctlHarness(t, "wri", 2, g.run, `return await agent("a", {vendor: "v", label: "leaf-a"});`)

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	directive(eng, "restart", a.JobID)
	// Attempt 1 dies on its ctx; attempt 2 starts and blocks on the gate again.
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.prompts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	g.release("a")
	v := <-vals
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	if s, _ := v.Export().(string); s != "ok:a#2" {
		t.Fatalf("result = %v, want attempt 2's answer", v.Export())
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("leaf ran %d times, want 2 (kill + re-exec)", n)
	}
}

// TestLeafStopSuccessWins: a stop directive landing after the attempt's work finished
// keeps the result — the directive is dropped, the leaf finalizes done, the meta the
// pre-mark flipped is normalized back.
func TestLeafStopSuccessWins(t *testing.T) {
	rec := &recorder{}
	var ignoreCtx sync.WaitGroup
	ignoreCtx.Add(1)
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		ignoreCtx.Wait() // ignores its ctx: the kill cannot take effect
		return subagent.Result{OK: true, Result: "survived"}
	})
	eng, vals, errs := ctlHarness(t, "wsw", 1, leaf, `return await agent("a", {vendor: "v", label: "leaf-a"});`)

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	directive(eng, "stop", a.JobID)
	jobByLabel(t, "leaf-a", "held") // the pre-mark is visible while the attempt still runs
	ignoreCtx.Done()                // now the attempt completes OK despite the cancel
	v := <-vals
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	if s, _ := v.Export().(string); s != "survived" {
		t.Fatalf("result = %v, want the surviving answer", v.Export())
	}
	// A fake leaf writes no terminal cache, so the on-disk normalization is not
	// observable here; the surviving value above is the success-wins proof (the
	// meta normalization has its own subagent-level tests).
}

// TestLeafDirectiveEdges: double-stop is idempotent, an unknown target narrates without
// effect, and a restart aimed at a still-queued attempt cancels and re-queues it (a
// user intent is never dropped on the queued/running race).
func TestLeafDirectiveEdges(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	eng, vals, errs := ctlHarness(t, "wde", 1, g.run, `
return await parallel([
    () => agent("a", {vendor: "v", label: "leaf-a"}),
    () => agent("b", {vendor: "v", label: "leaf-b"}),
]);`)

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	b := jobByLabel(t, "leaf-b", "queued") // pool of 1: b waits behind a
	directive(eng, "restart", b.JobID)     // queued → cancel-and-retry (never a dropped intent)
	directive(eng, "stop", a.JobID)
	directive(eng, "stop", a.JobID) // idempotent
	directive(eng, "stop", "no-such-leaf")
	jobByLabel(t, "leaf-a", "held")
	g.release("b")
	g.release("a")
	directive(eng, "restart", a.JobID)
	v := <-vals
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.Export().([]interface{})
	if got[0] != "ok:a#2" || got[1] != "ok:b#2" {
		t.Fatalf("results = %v (a restarted once; b requeued by its restart directive)", got)
	}
}

// TestRunStopReleasesHeldLeaf (integration point ②/③): cancelling a run with a held
// leaf releases the hold terminal-`stopped` — the drain terminates and nothing strands.
func TestRunStopReleasesHeldLeaf(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = g.run
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	ctx, cancel := context.WithCancel(context.Background())
	eng := newTestEngine(ctx, "wrh", 2)
	errs := make(chan error, 1)
	go func() {
		_, err := eng.run("c.js", []byte(`return await agent("a", {vendor: "v", label: "leaf-a"});`), Options{})
		errs <- err
	}()
	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	directive(eng, "stop", a.JobID)
	jobByLabel(t, "leaf-a", "held")
	cancel()
	select {
	case err := <-errs:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("err = %v, want run stopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return — a held leaf deadlocked the abort drain")
	}
	if res := subagent.StatusFor(a.JobID); res.Status != "stopped" {
		t.Errorf("held leaf after run stop = %q, want stopped", res.Status)
	}
}

// TestCtlPollerEndToEnd (W-cli slice): the real control plane — SendLeafCommand appends
// to runs/<id>.ctl, the engine's poller applies it — holds and then restarts a leaf
// through a full foreground Execute.
func TestCtlPollerEndToEnd(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = g.run
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })

	dir := t.TempDir()
	script := filepath.Join(dir, "w.js")
	src := "const meta = {name: \"n\", description: \"d\"};\nreturn await agent(\"a\", {vendor: \"v\", label: \"leaf-a\"});\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() { errs <- Execute(context.Background(), script, run.RunID, Options{}) }()

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	if err := SendLeafCommand(run.RunID, "stop", a.JobID); err != nil {
		t.Fatalf("send stop: %v", err)
	}
	jobByLabel(t, "leaf-a", "held")
	g.release("a")
	if err := SendLeafCommand(run.RunID, "restart", a.JobID); err != nil {
		t.Fatalf("send restart: %v", err)
	}
	select {
	case err := <-errs:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not complete after the restart directive")
	}
	got, _ := subagent.ReadRun(run.RunID)
	if got.Status != "done" {
		t.Errorf("run status = %q, want done", got.Status)
	}
	if res := subagent.StatusFor(a.JobID); res.Attempt != 2 {
		t.Errorf("leaf-a final attempt = %d, want 2", res.Attempt)
	}
}

// TestResumeDropsStaleHold (W-resume-drops-hold): a held meta left by a killed engine is
// normalized terminal-stopped at the next invocation's start.
func TestResumeDropsStaleHold(t *testing.T) {
	rec := &recorder{}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "r.js")
	if err := os.WriteFile(script, []byte("const meta = {name: \"n\", description: \"d\"};\nreturn 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	stale := subagent.MintQueuedLeaf(subagent.Request{Vendor: "v", RunID: run.RunID, Label: "stale"}, "m")
	subagent.HoldLeaf(stale)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res := subagent.StatusFor(stale); res.Status != "stopped" {
		t.Errorf("stale held leaf = %q, want stopped (holds are never persisted)", res.Status)
	}
}

// TestPhaseStopParksAndRestartResumes (W-phase-live): a phase stop holds the title's
// running member AND parks a member minted into the phase afterwards (before its first
// exec); the phase restart wakes both and the run completes.
func TestPhaseStopParksAndRestartResumes(t *testing.T) {
	rec := &recorder{}
	g := newGateLeaf(rec)
	eng, vals, errs := ctlHarness(t, "wpl", 2, g.run, `
phase("p1");
const a = agent("a", {vendor: "v", label: "leaf-a"});
const ra = await a;
const b = await agent("b", {vendor: "v", label: "leaf-b"});
return [ra, b];`)

	a := jobByLabel(t, "leaf-a", "queued")
	waitCalled(t, rec, "a")
	eng.post(leafCB{state: func() { eng.applyPhaseDirective(ctlCommand{Op: "stop-phase", Phase: "p1"}) }})
	jobByLabel(t, "leaf-a", "held")
	// Wake ONLY the leaf: it finishes, and the script then mints b INTO the held phase —
	// b must park before its first exec.
	g.release("a")
	directive(eng, "restart", a.JobID)
	b := jobByLabel(t, "leaf-b", "held")
	if got := rec.prompts(); len(got) != 2 { // a ran twice; b never executed
		t.Fatalf("prompts = %v — a parked leaf must not exec", got)
	}
	// Phase restart wakes the parked member; first spawn keeps attempt 1.
	g.release("b")
	eng.post(leafCB{state: func() { eng.applyPhaseDirective(ctlCommand{Op: "restart-phase", Phase: "p1"}) }})
	v := <-vals
	if err := <-errs; err != nil {
		t.Fatalf("run: %v", err)
	}
	got := v.Export().([]interface{})
	if got[0] != "ok:a#2" || got[1] != "ok:b#1" {
		t.Fatalf("results = %v, want [ok:a#2 ok:b#1]", got)
	}
	if res := subagent.StatusFor(b.JobID); res.Attempt > 1 {
		t.Errorf("parked leaf woke at attempt %d, want its FIRST attempt", res.Attempt)
	}
}

// TestPhaseRestartPlan (W-phase-terminal slice): the keyed phase restart drops the
// phase's whole key set and honestly names the phases a shared key widens into.
func TestPhaseRestartPlan(t *testing.T) {
	leaves := []subagent.Result{
		{Phase: "p1", JournalKey: "k1"},
		{Phase: "p1", JournalKey: "k2"},
		{Phase: "p2", JournalKey: "k1"}, // shares k1 with p1 → widening
		{Phase: "p3", JournalKey: "k9"},
		{Phase: "p1", JournalKey: ""}, // a failed leaf: never journaled, re-runs anyway
	}
	keys, widened := phaseRestartPlan(leaves, "p1")
	if !keys["k1"] || !keys["k2"] || len(keys) != 2 {
		t.Fatalf("keys = %v, want {k1,k2}", keys)
	}
	if len(widened) != 1 || widened[0] != "p2" {
		t.Fatalf("widened = %v, want [p2]", widened)
	}
}
