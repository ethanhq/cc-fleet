package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMintQueuedLeafThenStatusForQueued: a minted queued placeholder (PID=0, Status="queued", no
// result cache) is reported BY StatusFor as queued — not classified as a dead/failed empty leaf — and
// carries its run grouping. registerSyncJob flips the SAME id to running; finalize makes it done.
func TestMintQueuedLeafThenStatusForQueued(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	req := Request{Vendor: "v", Model: "m", RunID: "r1", Phase: "map", Label: "leaf-a", JournalKey: "k1"}
	jobID := MintQueuedLeaf(req, "m")
	if jobID == "" {
		t.Fatal("MintQueuedLeaf returned empty id")
	}
	st := StatusFor(jobID)
	if st.Status != "queued" {
		t.Fatalf("StatusFor on a queued placeholder = %q, want queued", st.Status)
	}
	if st.RunID != "r1" || st.Phase != "map" || st.Label != "leaf-a" {
		t.Errorf("queued placeholder lost its run grouping: %+v", st)
	}
	// Flip queued→running (subagent.Run reuses the id via registerSyncJob).
	if !registerSyncJob(jobID, req, "m", "", "") {
		t.Fatal("registerSyncJob(reuse) failed")
	}
	if st := StatusFor(jobID); st.Status != "running" {
		t.Fatalf("after registerSyncJob, status = %q, want running", st.Status)
	}
	// Finalize → done, terminal cache served.
	finalizeSyncJob(jobID, Result{OK: true, NumTurns: 1})
	if st := StatusFor(jobID); st.Status != "done" {
		t.Fatalf("after finalize, status = %q, want done", st.Status)
	}
}

// TestRegisterSyncJobClearsStaleCacheOnReuse: re-registering a reused job id drops a prior
// registration's terminal cache + answer so the board re-reads it as running, not the stale
// done; the carried Attempt updates.
func TestRegisterSyncJobClearsStaleCacheOnReuse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	req := Request{Vendor: "v", RunID: "r", Phase: "p", Label: "l", PersistIO: true, IOPrompt: "hi", Attempt: 1}
	jobID := MintQueuedLeaf(req, "m")
	registerSyncJob(jobID, req, "m", "", "")
	finalizeSyncJob(jobID, Result{OK: true, Result: "first-answer", NumTurns: 1}) // attempt 1 → done cache + .answer
	if StatusFor(jobID).Status != "done" {
		t.Fatal("attempt 1 should be cached done")
	}
	// Re-registering the SAME id must clear the stale done cache.
	req2 := req
	req2.Attempt = 2
	if !registerSyncJob(jobID, req2, "m", "", "") {
		t.Fatal("re-register failed")
	}
	st := StatusFor(jobID)
	if st.Status != "running" {
		t.Fatalf("after re-register, status = %q, want running (stale done cache cleared)", st.Status)
	}
	if st.Attempt != 2 {
		t.Errorf("Attempt after re-register = %d, want 2", st.Attempt)
	}
}

// TestFinalizeQueuedLeafFailed: an abandoned queued placeholder finalizes to a terminal failed leaf
// (no phantom queued ◌ row); a real error class on the passed Result is preserved, else canonical.
func TestFinalizeQueuedLeafFailed(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	jobID := MintQueuedLeaf(Request{Vendor: "v", RunID: "r", Phase: "p", Label: "l"}, "m")
	if StatusFor(jobID).Status != "queued" {
		t.Fatal("placeholder should start queued")
	}
	// A pre-flight vendor failure keeps its real error class.
	FinalizeQueuedLeafFailed(jobID, Result{OK: false, ErrorCode: ErrCodeUnknownVendor, Vendor: "v"})
	st := StatusFor(jobID)
	if st.Status != "failed" {
		t.Fatalf("after finalize, status = %q, want failed (no phantom queued)", st.Status)
	}
	if st.ErrorCode != ErrCodeUnknownVendor {
		t.Errorf("real error class lost: ErrorCode = %q, want %q", st.ErrorCode, ErrCodeUnknownVendor)
	}
	// An empty/OK Result falls back to a canonical SUBAGENT_FAILED.
	j2 := MintQueuedLeaf(Request{Vendor: "v", RunID: "r", Phase: "p", Label: "l2"}, "m")
	FinalizeQueuedLeafFailed(j2, Result{})
	if st := StatusFor(j2); st.Status != "failed" || st.ErrorCode != ErrCodeFailed {
		t.Errorf("canonical fallback: status=%q code=%q, want failed/%s", st.Status, st.ErrorCode, ErrCodeFailed)
	}
	FinalizeQueuedLeafFailed("", Result{}) // no-op on an empty id (engine minted nothing)
}

// TestWorkflowRunBudgetTokensRoundTrip: BudgetTokens survives a SaveRun→ReadRun manifest cycle
// (additive, omitempty) alongside BudgetUSD.
func TestWorkflowRunBudgetTokensRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run := WorkflowRun{RunID: "rt-tok", StartedAt: "2026-01-01T00:00:00Z", BudgetTokens: 123456, BudgetUSD: 7.5}
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRun("rt-tok")
	if err != nil {
		t.Fatal(err)
	}
	if got.BudgetTokens != 123456 || got.BudgetUSD != 7.5 {
		t.Errorf("budget round-trip: BudgetTokens=%d BudgetUSD=%v, want 123456 / 7.5", got.BudgetTokens, got.BudgetUSD)
	}
}

// TestPruneRunsSparesLiveDeletesDead: prune removes runs with no live engine (a crashed run still
// stuck "running", and a terminal run) while sparing a run whose detached engine is verifiably alive.
func TestPruneRunsSparesLiveDeletesDead(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	orig := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = orig })
	// The seam reports the LIVE run's detached-engine argv for any pid, so EngineAlive matches only the
	// run whose id is "live"; "crashed" (same pid, different id) and "terminal" (pid 0) read as not-alive.
	reuseGuardArgv = func(int) ([]string, bool) {
		return []string{"cc-fleet", "workflow", "run", "x.star", "--run-id", "live"}, true
	}
	runs := []WorkflowRun{
		{RunID: "live", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self},
		{RunID: "crashed", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self},
		{RunID: "terminal", StartedAt: "2026-01-01T00:00:00Z", Status: "done"},
		// A foreground / pre-stamp run: still "running" but with no reapable pid — prune must NOT
		// delete it (it could be writing live), mirroring the resume/restart fail-closed guard.
		{RunID: "foreground", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0},
	}
	for _, r := range runs {
		if err := SaveRun(r); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := PruneRuns()
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (crashed + terminal pruned; live + foreground spared)", removed)
	}
	for _, id := range []string{"live", "foreground"} {
		if _, err := ReadRun(id); err != nil {
			t.Errorf("run %q must be spared, got %v", id, err)
		}
	}
	for _, id := range []string{"crashed", "terminal"} {
		if _, err := ReadRun(id); err == nil {
			t.Errorf("run %q should have been pruned", id)
		}
	}
}

// TestGCKeepsQueuedPlaceholderUntilTerminal: a `subagent-gc --older-than 0s` (the board's "clear done"
// sweep, cutoff = now) must NOT remove an active queued placeholder — it has no result cache and PID=0,
// but it is a not-yet-started leaf, not finished. Once finalized terminal it becomes GC-eligible.
func TestGCKeepsQueuedPlaceholderUntilTerminal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	jobID := MintQueuedLeaf(Request{Vendor: "v", RunID: "r", Phase: "p", Label: "l"}, "m")
	GC(0) // no age limit
	if st := StatusFor(jobID); st.Status != "queued" {
		t.Fatalf("GC removed an active queued placeholder: status = %q, want queued", st.Status)
	}
	FinalizeQueuedLeafFailed(jobID, Result{}) // now terminal (has a result cache)
	GC(0)
	if st := StatusFor(jobID); st.OK || st.ErrorCode != ErrCodeBadArgs {
		t.Errorf("a finalized queued leaf should be GC-reaped; StatusFor = {status:%q code:%q}, want unknown-job", st.Status, st.ErrorCode)
	}
}

// TestStatusForDeadBackgroundWritesAnswer: the dead-background terminal classification is a
// detached job's only finalizer — when the job opted into PersistIO it must persist the
// .answer side file alongside the result cache, or the board's detail card has no Output.
func TestStatusForDeadBackgroundWritesAnswer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const jobID = "deadbg00-0000-0000-0000-000000000000"
	meta := jobMeta{JobID: jobID, PID: 1 << 30, PGID: 1 << 30, Vendor: "v", Model: "m",
		StartedAt: "2026-06-01T00:00:00Z", Status: "running", JSON: true, PersistIO: true}
	if err := writeMetaFn(dir, meta); err != nil {
		t.Fatal(err)
	}
	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"BG-ANSWER","num_turns":1}`
	if err := os.WriteFile(filepath.Join(dir, jobID+".out"), []byte(envelope+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := StatusFor(jobID)
	if st.Status != "done" || st.Result != "BG-ANSWER" {
		t.Fatalf("dead-bg classification = %q/%q, want done/BG-ANSWER", st.Status, st.Result)
	}
	b, err := os.ReadFile(filepath.Join(dir, jobID+".answer"))
	if err != nil || string(b) != "BG-ANSWER" {
		t.Fatalf(".answer side file = %q, err=%v — the dead-bg finalizer must persist it", b, err)
	}
}
