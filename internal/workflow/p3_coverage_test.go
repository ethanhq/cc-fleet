package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestBackgroundTimeout: a background leaf that never finishes is bounded by its timeout=
// at wait() (which reaps it), rather than blocking forever.
func TestBackgroundTimeout(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, JobID: "j1", Status: "running"}
	})
	t.Cleanup(func() { runLeaf = old })
	oldS := statusForFn
	statusForFn = func(string) subagent.Result { return subagent.Result{OK: true, Status: "running"} } // never terminal
	t.Cleanup(func() { statusForFn = oldS })

	eng := &engine{sched: newScheduler(context.Background(), 2), runID: "bgt"}
	_, err := eng.run("bgt.star", `
h = agent("a", vendor="v", run_in_background=True, timeout=0.3)
r = wait(h)
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a background leaf must honor timeout= at wait(), got %v", err)
	}
}

// TestEffectiveModelJournalKey: a no-model leaf is keyed under the EFFECTIVE model
// (meta.model), so an unchanged resume hits the cache and changing meta.model busts it.
func TestEffectiveModelJournalKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	dir := t.TempDir()
	script := filepath.Join(dir, "m.star")
	write := func(model string) {
		src := `meta = {"name": "n", "description": "d", "model": "` + model + `"}
r = agent("q", vendor="v")
`
		if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("X")
	run, _ := Prepare(script)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("run1 calls = %d, want 1", n)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil { // resume, same model
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("unchanged meta.model resume should hit cache, calls = %d want 1", n)
	}
	write("Y") // effective model changes → the no-model leaf's key changes → re-run
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("resume after model change: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("changing meta.model must bust the cache, calls = %d want 2", n)
	}
}

// TestBackgroundResume: a backgrounded leaf journaled on its first wait() is served from
// the journal on resume (a born-resolved handle) — zero re-launch.
func TestBackgroundResume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, JobID: "j-" + c.prompt, Status: "running"}
	})
	t.Cleanup(func() { runLeaf = old })
	oldS := statusForFn
	statusForFn = func(jobID string) subagent.Result {
		return subagent.Result{OK: true, Status: "done", Result: "ans:" + jobID}
	}
	t.Cleanup(func() { statusForFn = oldS })

	const runID = "bgr"
	src := `h = agent("a", vendor="v", run_in_background=True)
r = wait(h)`
	if _, err := newEngineFor(t, runID, 2).run("bgr.star", src, Options{}); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("run1 launches = %d, want 1", n)
	}
	if _, err := newEngineFor(t, runID, 2).run("bgr.star", src, Options{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("resume should NOT re-launch the background leaf, launches = %d want 1", n)
	}
}

// TestBudgetCountsFailedSchemaLeaf: an OK exec's cost is counted even when its
// structured payload then fails validation — the spend happened; the failure degrades
// to None under parallel and budget.spent() reflects it.
func TestBudgetCountsFailedSchemaLeaf(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"b": 2}`), CostUSD: 0.3}
	})
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "brt", budgetTotal: 100}
	g, err := eng.run("brt.star", `
r = parallel([lambda: agent("q", vendor="v", schema={"required": ["a"]})])
failed = r[0] == None
sp = budget.spent()
`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if g["failed"] != starlark.Bool(true) {
		t.Error("the schema-failing leaf should degrade to None under parallel")
	}
	if f, _ := starlark.AsFloat(g["sp"]); f != 0.3 {
		t.Errorf("budget spent = %v, want 0.3 (the OK exec's cost counts despite the schema failure)", g["sp"])
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("leaf ran %d times, want 1", n)
	}
}

// TestWorktreeCleanupOnLeafFailure: a failing worktree leaf still tears down its worktree;
// a worktree-create error surfaces as an agent error.
func TestWorktreeCleanupOnLeafFailure(t *testing.T) {
	rec := &recorder{}
	old := runLeaf
	runLeaf = fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "boom"}
	})
	t.Cleanup(func() { runLeaf = old })

	cleaned := false
	oldW := createWorktreeFn
	createWorktreeFn = func(string) (string, func(), error) { return "/tmp/wt", func() { cleaned = true }, nil }
	t.Cleanup(func() { createWorktreeFn = oldW })
	if _, err := runScript(t, "wtf", 1, runLeaf, `x = agent("edit", vendor="v", isolation="worktree")`); err == nil {
		t.Error("a failing worktree leaf should surface an error")
	}
	if !cleaned {
		t.Error("the worktree must be torn down even when the leaf fails")
	}

	createWorktreeFn = func(string) (string, func(), error) { return "", nil, context.DeadlineExceeded }
	if _, err := runScript(t, "wtc", 1, runLeaf, `x = agent("edit", vendor="v", isolation="worktree")`); err == nil {
		t.Error("a worktree-create failure must surface as an agent error")
	}
}
