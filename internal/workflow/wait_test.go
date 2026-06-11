package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// plantLeaf mints a queued leaf tagged with the run; held=true parks it (the
// hold-protocol meta state an operator's stop --leaf produces).
func plantLeaf(t *testing.T, runID, label string, held bool) string {
	t.Helper()
	id := subagent.MintQueuedLeaf(subagent.Request{RunID: runID, Label: label, Provider: "p"}, "m")
	if id == "" {
		t.Fatal("MintQueuedLeaf returned no id")
	}
	if held {
		subagent.HoldLeaf(id)
	}
	return id
}

// TestWaitTerminalRun: an already-terminal run returns on the first poll with the
// status-matching outcome (idempotent re-arm), and a failed run keeps its status.
func TestWaitTerminalRun(t *testing.T) {
	for _, status := range []string{"done", "failed", "stopped"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			run, err := subagent.NewRunWithMeta("n", "d", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			run.Status = status
			if err := subagent.SaveRun(run); err != nil {
				t.Fatal(err)
			}
			res, werr := Wait(context.Background(), run.RunID, WaitOptions{Interval: 5 * time.Millisecond})
			if werr != nil {
				t.Fatalf("Wait returned %v, want nil for a terminal run", werr)
			}
			if res.Outcome != WaitTerminal || res.Run.Status != status {
				t.Errorf("outcome=%s status=%q, want terminal/%s", res.Outcome, res.Run.Status, status)
			}
		})
	}
}

// TestWaitEngineGone: a stale "running" manifest whose detached engine is dead returns
// ErrEngineGone instead of blocking forever.
func TestWaitEngineGone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	run.EnginePID = 0x7ffffffe // not us and (effectively) not alive
	if err := subagent.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	res, werr := Wait(context.Background(), run.RunID, WaitOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(werr, ErrEngineGone) {
		t.Fatalf("Wait returned %v, want ErrEngineGone", werr)
	}
	if res.Outcome != WaitEngineGone {
		t.Errorf("outcome = %s, want engine_gone", res.Outcome)
	}
}

// TestWaitParked: a running run (EnginePID 0 — the foreground case, which must also
// fire) whose only leaf is held debounces to parked, with the held leaf named.
func TestWaitParked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil) // Status running, EnginePID 0
	if err != nil {
		t.Fatal(err)
	}
	heldID := plantLeaf(t, run.RunID, "w1", true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, werr := Wait(ctx, run.RunID, WaitOptions{Interval: 5 * time.Millisecond})
	if werr != nil {
		t.Fatalf("Wait returned %v, want nil for a parked run", werr)
	}
	if res.Outcome != WaitParked {
		t.Fatalf("outcome = %s, want parked", res.Outcome)
	}
	if res.Counts.Held != 1 || len(res.Held) != 1 || res.Held[0].JobID != heldID || res.Held[0].Label != "w1" {
		t.Errorf("held snapshot = %+v (counts %+v), want the planted leaf", res.Held, res.Counts)
	}
}

// TestWaitQueuedBlocksParked: a queued leaf (a restart-in-flight, or a leaf waiting on
// a pool slot) breaks the parked predicate — Wait keeps waiting until ctx expires and
// reports a timeout heartbeat with the tally.
func TestWaitQueuedBlocksParked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plantLeaf(t, run.RunID, "h", true)
	plantLeaf(t, run.RunID, "q", false)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	res, werr := Wait(ctx, run.RunID, WaitOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(werr, context.DeadlineExceeded) {
		t.Fatalf("Wait returned %v, want DeadlineExceeded (queued leaf must block parked)", werr)
	}
	if res.Outcome != WaitTimeout || res.Counts.Held != 1 || res.Counts.Queued != 1 {
		t.Errorf("outcome=%s counts=%+v, want timeout with held=1 queued=1", res.Outcome, res.Counts)
	}
}

// TestWaitNoLeavesNotParked: a running run with zero job files (the resume journal-replay
// window, or a script before its first agent()) never reads parked — only a timeout.
func TestWaitNoLeavesNotParked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res, werr := Wait(ctx, run.RunID, WaitOptions{Interval: 5 * time.Millisecond})
	if !errors.Is(werr, context.DeadlineExceeded) || res.Outcome != WaitTimeout {
		t.Fatalf("Wait returned (%s, %v), want a timeout heartbeat", res.Outcome, werr)
	}
}

// TestWaitTimeoutJustFinishedGuard: a run that flips terminal while Wait sleeps is
// reported terminal, not misread as a timeout (the ctx.Done re-read).
func TestWaitTimeoutJustFinishedGuard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		run.Status = "done"
		_ = subagent.SaveRun(run)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// One slow tick: the first poll sees running, the ctx fires before the second,
	// and the re-read must catch the flip to done.
	res, werr := Wait(ctx, run.RunID, WaitOptions{Interval: 5 * time.Second})
	if werr != nil || res.Outcome != WaitTerminal || res.Run.Status != "done" {
		t.Fatalf("Wait returned (%s/%s, %v), want terminal/done via the ctx re-read", res.Outcome, res.Run.Status, werr)
	}
}

// TestWaitUnknownRun: an unknown id fails on the first poll (path-free not-found).
func TestWaitUnknownRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, werr := Wait(context.Background(), "no-such-run", WaitOptions{Interval: 5 * time.Millisecond})
	if werr == nil {
		t.Fatal("Wait succeeded for an unknown run id")
	}
}

// TestWaitPollDebounce exercises the parked counter directly (deterministic — no
// timing): held-only ticks accumulate, any queued tick resets, and the threshold
// fires only after waitParkedTicks consecutive holds.
func TestWaitPollDebounce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	plantLeaf(t, run.RunID, "h", true)

	parked := 0
	for i := 0; i < waitParkedTicks-1; i++ {
		if _, stop, _ := waitPoll(run.RunID, &parked); stop {
			t.Fatalf("parked fired after %d ticks, want %d", i+1, waitParkedTicks)
		}
	}
	// A queued leaf appears (restart-in-flight): the streak resets.
	qID := plantLeaf(t, run.RunID, "q", false)
	if _, stop, _ := waitPoll(run.RunID, &parked); stop || parked != 0 {
		t.Fatalf("queued tick: stop=%v parked=%d, want streak reset", stop, parked)
	}
	// The queued leaf settles; a full fresh streak is required before parked fires.
	subagent.FinalizeQueuedLeafFailed(qID, subagent.Result{})
	for i := 0; i < waitParkedTicks-1; i++ {
		if _, stop, _ := waitPoll(run.RunID, &parked); stop {
			t.Fatalf("parked fired after %d post-reset ticks, want %d", i+1, waitParkedTicks)
		}
	}
	res, stop, werr := waitPoll(run.RunID, &parked)
	if !stop || werr != nil || res.Outcome != WaitParked {
		t.Fatalf("tick %d: stop=%v outcome=%s err=%v, want parked", waitParkedTicks, stop, res.Outcome, werr)
	}
	if res.Counts.Failed != 1 || res.Counts.Held != 1 {
		t.Errorf("counts = %+v, want held=1 failed=1", res.Counts)
	}
}

// TestRenderWaitLine: the human line carries the tally, scrubs control bytes, points a
// failed run at `workflow status` (never the raw Error), and names held leaves.
func TestRenderWaitLine(t *testing.T) {
	res := WaitResult{
		Run:     subagent.WorkflowRun{RunID: "r1", Status: "failed", Error: "LEAKED_PROVIDER_REPLY"},
		Outcome: WaitTerminal,
		Counts:  WaitCounts{Failed: 2},
	}
	line := RenderWaitLine(res)
	if !strings.Contains(line, "workflow status") || strings.Contains(line, "LEAKED_PROVIDER_REPLY") {
		t.Errorf("failed line must point at workflow status and never echo Error:\n%s", line)
	}

	parked := WaitResult{
		Run:     subagent.WorkflowRun{RunID: "r2", Status: "running"},
		Outcome: WaitParked,
		Counts:  WaitCounts{Held: 1},
		Held:    []WaitHeldLeaf{{JobID: "j1", Label: "w\x1b[31m1"}},
	}
	pline := RenderWaitLine(parked)
	if !strings.Contains(pline, "held:") || !strings.Contains(pline, "restart") {
		t.Errorf("parked line must name held leaves + the restart hint:\n%s", pline)
	}
	if strings.Contains(pline, "\x1b") {
		t.Errorf("control bytes must be scrubbed:\n%s", pline)
	}
}
