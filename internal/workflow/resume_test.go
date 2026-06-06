package workflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// newEngineFor builds an engine bound to runID with its on-disk journal loaded — the
// real resume substrate (a fresh engine per "invocation", sharing the journal file).
func newEngineFor(t *testing.T, runID string, concurrency int) *engine {
	t.Helper()
	jp, err := subagent.RunJournalPath(runID)
	if err != nil {
		t.Fatalf("journal path: %v", err)
	}
	return &engine{sched: newScheduler(context.Background(), concurrency), runID: runID, journal: loadJournal(jp)}
}

// TestResumeServesJournaledLeavesNoReexec: a second invocation of the same script under
// the same run id serves EVERY leaf from the journal — zero re-exec — and the results
// are byte-identical to the first run (determinism holds; same script+args ⇒ 100% hits).
func TestResumeServesJournaledLeavesNoReexec(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-1"
	src := `r = [agent("a", vendor="v"), agent("b", vendor="v"), agent("c", vendor="v")]`

	g1, err := newEngineFor(t, runID, 4).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Fatalf("pass1 leaf calls = %d, want 3", n)
	}

	g2, err := newEngineFor(t, runID, 4).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("cumulative leaf calls after resume = %d, want 3 (all served from the journal)", n)
	}
	l1, l2 := wantStringList(t, g1, "r"), wantStringList(t, g2, "r")
	for i := range l1 {
		if asStr(t, l1[i]) != asStr(t, l2[i]) {
			t.Errorf("result[%d] changed across resume: %q vs %q", i, asStr(t, l1[i]), asStr(t, l2[i]))
		}
	}
}

// TestFreshRunDuplicateCallsBothExecute: two identical agent() calls in the SAME fresh
// run BOTH execute — the journal serves prior-run results only, so the second call is
// not silently memoized against the first (which would collapse a sampled-prompt loop
// and diverge from native).
func TestFreshRunDuplicateCallsBothExecute(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	// Three identical serial calls in one fresh run.
	g, err := newEngineFor(t, "dup-1", 1).run("d.star",
		`r = [agent("same", vendor="v"), agent("same", vendor="v"), agent("same", vendor="v")]`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("duplicate calls executed %d times, want 3 (no within-run memoization)", n)
	}
	if l := wantStringList(t, g, "r"); len(l) != 3 {
		t.Fatalf("got %d results, want 3", len(l))
	}
}

// TestResumeEditedLeafReruns: editing ONE leaf's prompt and re-running re-executes only
// that leaf (its key is now absent); the unchanged leaves hit the journal.
func TestResumeEditedLeafReruns(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-2"
	if _, err := newEngineFor(t, runID, 4).run("r.star",
		`r = [agent("a", vendor="v"), agent("b", vendor="v")]`, Options{}); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Fatalf("pass1 calls = %d, want 2", n)
	}
	// Edit leaf b → b2; a is unchanged.
	if _, err := newEngineFor(t, runID, 4).run("r.star",
		`r = [agent("a", vendor="v"), agent("b2", vendor="v")]`, Options{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("cumulative calls = %d, want 3 (only the edited leaf re-ran)", n)
	}
	if last := rec.prompts()[2]; last != "b2" {
		t.Errorf("re-run leaf prompt = %q, want b2", last)
	}
}

// TestResumeDuplicateKeyCrashRecovery: the loop-until-dry shape — the SAME prompt called
// N times (one content key, N journal entries). A run killed after 1 of 3 iterations
// journals one entry; on resume the first call pops it (cached) and iterations 2-3 find
// the queue exhausted and RE-RUN. A single-key map would wrongly serve all 3 from the one
// entry and skip the unrun tail.
func TestResumeDuplicateKeyCrashRecovery(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-dup"
	jp, _ := subagent.RunJournalPath(runID)
	// The leaves are pinned profile="full" so the seeded key carries no slim shape (and no
	// host-claude-version dependence).
	loadJournal(jp).append(journalKey("v", "", "same", "", "", "", nil, false, false), "ok:same") // 1 of 3 done before the kill

	g, err := newEngineFor(t, runID, 1).run("d.star",
		`r = [agent("same", vendor="v", profile="full"), agent("same", vendor="v", profile="full"), agent("same", vendor="v", profile="full")]`, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("dup-key crash recovery executed %d leaves, want 2 (1 cached + the 2 unrun re-run)", n)
	}
	if l := wantStringList(t, g, "r"); len(l) != 3 {
		t.Fatalf("got %d results, want 3", len(l))
	}
}

// TestResumeCrashRecoveryPartialJournal: a run killed after leaf "a" (its result
// journaled) re-runs only "b" and "c" on resume; "a" is served from cache.
func TestResumeCrashRecoveryPartialJournal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-3"
	jp, _ := subagent.RunJournalPath(runID)
	// Seed the journal as if the run finished "a" then was killed (key MUST match the
	// engine's: vendor "v", model "", prompt "a", no schema; the leaves are pinned
	// profile="full" so the key carries no slim shape).
	loadJournal(jp).append(journalKey("v", "", "a", "", "", "", nil, false, false), "ok:a")

	g, err := newEngineFor(t, runID, 4).run("r.star",
		`r = [agent("a", vendor="v", profile="full"), agent("b", vendor="v", profile="full"), agent("c", vendor="v", profile="full")]`, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("calls = %d, want 2 (a served from the journal, b+c run)", n)
	}
	for _, p := range rec.prompts() {
		if p == "a" {
			t.Error("leaf a was re-executed despite being journaled")
		}
	}
	if l := wantStringList(t, g, "r"); asStr(t, l[0]) != "ok:a" {
		t.Errorf("cached a result = %q, want ok:a", asStr(t, l[0]))
	}
}

// TestResumeFailedLeafNotCached: a leaf that FAILED is not journaled, so resume re-runs
// it (and can then succeed) rather than serving a nonexistent cache.
func TestResumeFailedLeafNotCached(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	var mu sync.Mutex
	seen := map[string]bool{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "flaky" {
			mu.Lock()
			first := !seen["flaky"]
			seen["flaky"] = true
			mu.Unlock()
			if first {
				return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "boom"}
			}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-4"
	src := `r = parallel([lambda: agent("flaky", vendor="v")])` // failure → None, not journaled

	g1, err := newEngineFor(t, runID, 2).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if l := wantStringList(t, g1, "r"); l[0] != starlark.None {
		t.Fatalf("failed leaf should degrade to None, got %v", l[0])
	}
	g2, err := newEngineFor(t, runID, 2).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if l := wantStringList(t, g2, "r"); asStr(t, l[0]) != "ok:flaky" {
		t.Error("a previously-failed leaf must re-run on resume (no cache), not stay None")
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("flaky should have run twice (fail, then succeed on resume), got %d", n)
	}
}

// TestResumeSchemaCorruptCacheFallsThrough: a schema leaf whose journaled payload no
// longer validates (a corrupt/hand-edited journal) is NOT served from cache — replay
// falls through and re-runs the leaf rather than aborting the run.
func TestResumeSchemaCorruptCacheFallsThrough(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"answer": 9}`)}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-corrupt"
	src := `res = agent("q", vendor="v", schema={"required": ["answer"]})
ans = res["answer"]`

	if _, err := newEngineFor(t, runID, 1).run("r.star", src, Options{}); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("pass1 calls = %d, want 1", n)
	}
	// Corrupt the cached result (keep the key) so re-validation fails on resume.
	jp, _ := subagent.RunJournalPath(runID)
	data, _ := os.ReadFile(jp)
	var e journalEntry
	if err := json.Unmarshal([]byte(firstLine(data)), &e); err != nil {
		t.Fatalf("parse journal line: %v", err)
	}
	corrupt, _ := json.Marshal(journalEntry{Key: e.Key, Result: "not json at all"})
	if err := os.WriteFile(jp, append(corrupt, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := newEngineFor(t, runID, 1).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("a corrupt schema cache must re-run the leaf: calls = %d, want 2", n)
	}
	if i, _ := starlark.AsInt32(g["ans"]); i != 9 {
		t.Errorf("re-run produced ans = %v, want 9", g["ans"])
	}
}

// firstLine returns the first newline-delimited line of b (trimmed of the trailing \n).
func firstLine(b []byte) string {
	for i, c := range b {
		if c == '\n' {
			return string(b[:i])
		}
	}
	return string(b)
}

// TestLaunchResumeWiring covers the user-facing --resume surface end to end: Launch
// rejects a nonexistent / path-unsafe resume id, and a foreground fresh-run-then-resume
// replays from the journal (no re-exec) through the real Launch→Execute→journal-load path.
func TestLaunchResumeWiring(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	ctx := context.Background()

	dir := t.TempDir()
	script := filepath.Join(dir, "w.star")
	if err := os.WriteFile(script, []byte("meta = {\"name\": \"n\", \"description\": \"d\"}\nx = agent(\"a\", vendor=\"v\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Launch(ctx, script, Options{Resume: "no-such-run"}, true); err == nil {
		t.Error("resume of a nonexistent run id must error")
	}
	if _, err := Launch(ctx, script, Options{Resume: "../evil"}, true); err == nil {
		t.Error("resume with a path-unsafe id must error before any execution")
	}

	id, err := Launch(ctx, script, Options{}, true)
	if err != nil {
		t.Fatalf("fresh foreground run: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("fresh run leaf calls = %d, want 1", n)
	}
	if _, err := Launch(ctx, script, Options{Resume: id}, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("resume re-ran the leaf: calls = %d, want 1 (served from the journal)", n)
	}
}

// TestResumeSchemaLeafReplaysWithoutExec: a schema leaf's cached payload (the journaled
// structured output) is re-decoded + re-validated on resume (deterministic), serving the
// validated value without a vendor exec.
func TestResumeSchemaLeafReplaysWithoutExec(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "prose", StructuredOutput: json.RawMessage(`{"answer": 5}`)}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-5"
	src := `res = agent("q", vendor="v", schema={"required": ["answer"]})
ans = res["answer"]`

	g1, err := newEngineFor(t, runID, 2).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if i, _ := starlark.AsInt32(g1["ans"]); i != 5 {
		t.Fatalf("ans = %v, want 5", g1["ans"])
	}
	if n := len(rec.prompts()); n != 1 {
		t.Fatalf("pass1 calls = %d, want 1", n)
	}
	g2, err := newEngineFor(t, runID, 2).run("r.star", src, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if i, _ := starlark.AsInt32(g2["ans"]); i != 5 {
		t.Errorf("resumed ans = %v, want 5 (re-validated from cache)", g2["ans"])
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("schema leaf re-executed on resume: calls = %d, want 1", n)
	}
}

// TestResumeSchemaPreV2TextReplay: a journal written before structured outputs may hold
// the leaf's TEXT answer (possibly markdown-fenced) under a schema leaf's key; replay
// still decodes + validates it and serves the value without an exec.
func TestResumeSchemaPreV2TextReplay(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "resume-prev2"
	jp, _ := subagent.RunJournalPath(runID)
	// The leaf is pinned profile="full" so the seeded key carries no slim shape (and no
	// host-claude-version dependence). The value is the old-style fenced text JSON.
	loadJournal(jp).append(journalKey("v", "", "q", `{"required":["answer"]}`, "", "full", nil, false, false),
		"```json\n{\"answer\": 5}\n```")

	g, err := newEngineFor(t, runID, 1).run("r.star",
		`res = agent("q", vendor="v", profile="full", schema={"required": ["answer"]})
ans = res["answer"]`, Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n := len(rec.prompts()); n != 0 {
		t.Errorf("a pre-v2 cached schema leaf re-executed: calls = %d, want 0", n)
	}
	if i, _ := starlark.AsInt32(g["ans"]); i != 5 {
		t.Errorf("replayed ans = %v, want 5", g["ans"])
	}
}
