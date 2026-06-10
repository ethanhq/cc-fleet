package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestBackgroundTimeout: a background leaf is bounded by its own timeout — the option
// reaches the exec as Request.Timeout, and an overrun surfaces as the leaf's
// SUBAGENT_TIMEOUT failure when the saved promise is awaited.
func TestBackgroundTimeout(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "SUBAGENT_TIMEOUT", ErrorMsg: "timed out"}
	})
	_, err := runScript(t, "bgt", 2, leaf, `
const p = agent("a", {provider: "v", timeout: 0.3});
return await p;
`)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("a timed-out background leaf must reject its await, got %v", err)
	}
	if c := rec.snapshot()[0]; c.timeout != 300*time.Millisecond {
		t.Errorf("timeout = %v, want 300ms plumbed to the exec", c.timeout)
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
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	dir := t.TempDir()
	script := filepath.Join(dir, "m.js")
	write := func(model string) {
		src := `const meta = {name: "n", description: "d", model: "` + model + `"};
await agent("q", {provider: "v"});
`
		if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("X")
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
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

// TestBackgroundResume: a background leaf journaled at its completion is served from
// the journal on resume (an already-resolved promise) — zero re-launch.
func TestBackgroundResume(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "bgr"
	src := `const p = agent("a", {provider: "v"});
return await p;`
	if _, err := newEngineFor(t, runID, 2).run("bgr.js", []byte(src), Options{}); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("run1 launches = %d, want 1", n)
	}
	if _, err := newEngineFor(t, runID, 2).run("bgr.js", []byte(src), Options{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("resume should NOT re-launch the background leaf, launches = %d want 1", n)
	}
}

// TestBudgetCountsFailedSchemaLeaf: an OK exec's cost is counted even when its
// structured payload then fails validation — the spend happened; the failure degrades
// to null under parallel and budget.spent() reflects it.
func TestBudgetCountsFailedSchemaLeaf(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"b": 2}`), CostUSD: 0.3}
	})
	eng := budgetEngine(t, "brt", 1, leaf)
	eng.budgetTotal = 100
	v, err := eng.run("brt.js", []byte(`
const r = await parallel([() => agent("q", {provider: "v", schema: {required: ["a"]}})]);
return { failed: r[0] === null, sp: budget.spent() };
`), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if m["failed"] != true {
		t.Error("the schema-failing leaf should degrade to null under parallel")
	}
	if f := budgetFloat(t, m, "sp"); f != 0.3 {
		t.Errorf("budget spent = %v, want 0.3 (the OK exec's cost counts despite the schema failure)", f)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("leaf ran %d times, want 1", n)
	}
}

// TestWorktreeCleanupOnLeafFailure: a failing worktree leaf still tears down its worktree;
// a worktree-create error surfaces as an agent error.
func TestWorktreeCleanupOnLeafFailure(t *testing.T) {
	rec := &recorder{}
	failing := fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "boom"}
	})
	cleaned := false
	oldW := createWorktreeFn
	createWorktreeFn = func(string) (string, func(), error) { return "/tmp/wt", func() { cleaned = true }, nil }
	t.Cleanup(func() { createWorktreeFn = oldW })
	if _, err := runScript(t, "wtf", 1, failing, `return await agent("edit", {provider: "v", isolation: "worktree"});`); err == nil {
		t.Error("a failing worktree leaf should surface an error")
	}
	if !cleaned {
		t.Error("the worktree must be torn down even when the leaf fails")
	}

	createWorktreeFn = func(string) (string, func(), error) { return "", nil, context.DeadlineExceeded }
	if _, err := runScript(t, "wtc", 1, failing, `return await agent("edit", {provider: "v", isolation: "worktree"});`); err == nil {
		t.Error("a worktree-create failure must surface as an agent error")
	}
}
