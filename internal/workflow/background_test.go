package workflow

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestBackgroundAwait: agent(run_in_background=True) returns a handle immediately (the
// leaf launches detached), and wait() blocks for the result(s) — both a list of handles
// and a single handle.
func TestBackgroundAwait(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		// The background LAUNCH returns a job handle (running), not the result.
		return subagent.Result{OK: true, JobID: "job-" + c.prompt, Status: "running"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	oldS := statusForFn
	statusForFn = func(jobID string) subagent.Result {
		return subagent.Result{OK: true, Status: "done", Result: "ans:" + jobID, CostUSD: 0.1}
	}
	t.Cleanup(func() { statusForFn = oldS })

	g, err := runScript(t, "bg", 4, leaf, `
h1 = agent("a", vendor="v", run_in_background=True)
h2 = agent("b", vendor="v", run_in_background=True)
both = wait([h1, h2])
one = wait(agent("c", vendor="v", run_in_background=True))
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := wantStringList(t, g, "both")
	if len(got) != 2 || asStr(t, got[0]) != "ans:job-a" || asStr(t, got[1]) != "ans:job-b" {
		t.Errorf("wait([h1,h2]) = %v, want [ans:job-a ans:job-b]", got)
	}
	if asStr(t, g["one"]) != "ans:job-c" {
		t.Errorf("wait(single) = %v, want ans:job-c", g["one"])
	}
	// Three background launches (a, b, c) went through the leaf.
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("background launches = %d, want 3", n)
	}
}

// TestBackgroundSchemaRejected: schema= with run_in_background=True is an error (a
// background leaf's result is read back from its job file, and the structured payload
// is in-process only).
func TestBackgroundSchemaRejected(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "bgs", 2, echoLeaf(rec),
		`x = agent("q", vendor="v", schema={"required": ["a"]}, run_in_background=True)`)
	if err == nil || !strings.Contains(err.Error(), "run_in_background") {
		t.Errorf("expected a schema+background rejection, got %v", err)
	}
}

// TestBackgroundWaitTwiceNoDoubleCount: a second wait() on the same handle returns the
// cached result without re-polling, double-counting cost, or re-journaling.
func TestBackgroundWaitTwiceNoDoubleCount(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, JobID: "j1", Status: "running"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	var polls int32
	oldS := statusForFn
	statusForFn = func(string) subagent.Result {
		atomic.AddInt32(&polls, 1)
		return subagent.Result{OK: true, Status: "done", Result: "ans", CostUSD: 0.5}
	}
	t.Cleanup(func() { statusForFn = oldS })

	eng := &engine{sched: newScheduler(context.Background(), 2), runID: "bgw", budgetTotal: 100}
	g, err := eng.run("bgw.star", `
h = agent("a", vendor="v", run_in_background=True)
r1 = wait(h)
r2 = wait(h)
sp = budget.spent()
`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asStr(t, g["r1"]) != "ans" || asStr(t, g["r2"]) != "ans" {
		t.Errorf("both waits should return ans, got %v / %v", g["r1"], g["r2"])
	}
	if n := atomic.LoadInt32(&polls); n != 1 {
		t.Errorf("status polled %d times, want 1 (second wait served from the resolved handle)", n)
	}
	if f, _ := starlark.AsFloat(g["sp"]); f != 0.5 {
		t.Errorf("budget spent = %v, want 0.5 (cost counted once, not per wait)", g["sp"])
	}
}

// TestBackgroundWaitHonorsCancel: wait() on a never-finishing background job returns
// promptly with a cancellation error when the run ctx is cancelled, rather than hanging.
func TestBackgroundWaitHonorsCancel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, JobID: "j1", Status: "running"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	oldS := statusForFn
	statusForFn = func(string) subagent.Result { return subagent.Result{OK: true, Status: "running"} } // never terminal
	t.Cleanup(func() { statusForFn = oldS })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	eng := &engine{sched: newScheduler(ctx, 2), runID: "bgc"}
	_, err := eng.run("bgc.star", `
h = agent("a", vendor="v", run_in_background=True)
r = wait(h)
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("wait() must abort on cancellation, got %v", err)
	}
}

// TestBgDeadline_Backstop: a background leaf with NO timeout= (0 or negative) gets the default backstop
// deadline so an awaited leaf can never poll forever; an explicit timeout still wins. (The deadline's
// enforcement — overrun → reap → SUBAGENT_TIMEOUT — is exercised by TestBackgroundTimeout.)
func TestBgDeadline_Backstop(t *testing.T) {
	now := time.Now()
	if got := bgDeadline(now, 0); got != now.Add(defaultBgBackstop) {
		t.Fatalf("timeout=0 must use the backstop, got +%v", got.Sub(now))
	}
	if got := bgDeadline(now, -1); got != now.Add(defaultBgBackstop) {
		t.Fatalf("a negative timeout must use the backstop, got +%v", got.Sub(now))
	}
	if got := bgDeadline(now, 10); got != now.Add(10*time.Second) {
		t.Fatalf("an explicit timeout must win, got +%v", got.Sub(now))
	}
}
