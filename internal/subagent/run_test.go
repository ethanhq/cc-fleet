package subagent

import (
	"testing"
)

// TestNewRun_ReadRunRoundTrips: NewRun mints + persists a manifest that ReadRun
// reads back intact, including the ordered phase plan.
func TestNewRun_ReadRunRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	phases := []RunPhase{{Title: "build"}, {Title: "verify", Detail: "run the suite"}}
	run, err := NewRun("ship-it", phases)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if run.RunID == "" || run.StartedAt == "" || run.Status != "running" {
		t.Fatalf("NewRun returned an incomplete manifest: %+v", run)
	}

	got, err := ReadRun(run.RunID)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	if got.RunID != run.RunID || got.Name != "ship-it" || got.Status != "running" {
		t.Fatalf("ReadRun mismatch: %+v", got)
	}
	if len(got.Phases) != 2 || got.Phases[0].Title != "build" ||
		got.Phases[1].Title != "verify" || got.Phases[1].Detail != "run the suite" {
		t.Fatalf("ReadRun lost the phase plan: %+v", got.Phases)
	}
}

// TestReadRun_RejectsBadID: ReadRun validates the id before building a path, so a
// traversal arg can't read outside the runs dir.
func TestReadRun_RejectsBadID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := ReadRun("../escape"); err == nil {
		t.Fatal("ReadRun should reject a path-traversal run id")
	}
}

// TestListRuns_NewestFirstAndEmpty: empty (missing) runs dir → nil; after two
// NewRuns the list is newest-first by StartedAt.
func TestListRuns_NewestFirstAndEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	runs, err := ListRuns()
	if err != nil || runs != nil {
		t.Fatalf("ListRuns on empty: err=%v runs=%v, want nil,nil", err, runs)
	}

	r1, err := NewRun("first", nil)
	if err != nil {
		t.Fatalf("NewRun first: %v", err)
	}
	// Force a strictly later StartedAt so the ordering is deterministic regardless
	// of clock resolution (RFC3339 is whole-seconds).
	r2 := r1
	r2.RunID = "run-newer-id"
	r2.StartedAt = "2999-01-01T00:00:00Z"
	r2.Name = "second"
	writeRunForTest(t, r2)

	runs, err = ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRuns len = %d, want 2", len(runs))
	}
	if runs[0].Name != "second" {
		t.Fatalf("ListRuns not newest-first: %+v", runs)
	}
}

// TestRunStatus_FiltersJobsByRunID: RunStatus returns the manifest + only the jobs
// tagged with its run id. Unknown run → error.
func TestRunStatus_FiltersJobsByRunID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	run, err := NewRun("grouped", []RunPhase{{Title: "build"}})
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// Two jobs in this run, one in a different run — RunStatus must return only ours.
	if regSyncJob(Request{Vendor: "glm", RunID: run.RunID, Phase: "build", Label: "w1"}, "glm-4.6") == "" {
		t.Fatal("registerSyncJob w1 failed")
	}
	if regSyncJob(Request{Vendor: "glm", RunID: run.RunID, Phase: "build", Label: "w2"}, "glm-4.6") == "" {
		t.Fatal("registerSyncJob w2 failed")
	}
	if regSyncJob(Request{Vendor: "glm", RunID: "other-run", Phase: "x", Label: "x1"}, "glm-4.6") == "" {
		t.Fatal("registerSyncJob other failed")
	}

	gotRun, jobs, err := RunStatus(run.RunID)
	if err != nil {
		t.Fatalf("RunStatus: %v", err)
	}
	if gotRun.RunID != run.RunID {
		t.Fatalf("RunStatus manifest mismatch: %+v", gotRun)
	}
	if len(jobs) != 2 {
		t.Fatalf("RunStatus returned %d jobs, want 2 (filtered by run id)", len(jobs))
	}
	for _, j := range jobs {
		if j.RunID != run.RunID {
			t.Fatalf("RunStatus leaked a foreign job: %+v", j)
		}
	}

	if _, _, err := RunStatus("no-such-run"); err == nil {
		t.Fatal("RunStatus on an unknown run should error")
	}
}

// TestSaveRun_PreservesSessionAndOptions: the session id + replay options round-trip the manifest
// (the board groups by session; a restart resumes with the same args/persistIO/budget).
func TestSaveRun_PreservesSessionAndOptions(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := NewRun("r", nil)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.SessionID = "sess-xyz"
	run.ArgsJSON = `{"q":"hi"}`
	run.NoPersistIO = true
	run.BudgetUSD = 2.5
	if err := SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	got, err := ReadRun(run.RunID)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	if got.SessionID != "sess-xyz" || got.ArgsJSON != `{"q":"hi"}` || !got.NoPersistIO || got.BudgetUSD != 2.5 {
		t.Fatalf("manifest lost the session/replay options: %+v", got)
	}
}

// TestSyncJob_PersistsJournalKey: a sync leaf's journal key survives register → finalize → StatusFor
// so the board can target that single leaf for a restart.
func TestSyncJob_PersistsJournalKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	jobID := regSyncJob(Request{Vendor: "glm", RunID: "r1", Phase: "p", Label: "a", JournalKey: "key-abc"}, "glm-4.6")
	if jobID == "" {
		t.Fatal("registerSyncJob returned an empty id")
	}
	finalizeSyncJob(jobID, Result{OK: true, NumTurns: 1})
	if got := StatusFor(jobID); got.JournalKey != "key-abc" {
		t.Fatalf("JournalKey lost through the job lifecycle, got %q", got.JournalKey)
	}
}

// TestPurgeRun_RemovesRunAndJobs: PurgeRun deletes the manifest and every job tagged with the run.
func TestPurgeRun_RemovesRunAndJobs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := NewRun("r", nil)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	jobID := regSyncJob(Request{Vendor: "v", RunID: run.RunID, Phase: "p", Label: "a"}, "m")
	finalizeSyncJob(jobID, Result{OK: true})
	if got := StatusFor(jobID); got.RunID != run.RunID {
		t.Fatalf("setup: job not tagged with the run, got %+v", got)
	}
	if err := PurgeRun(run.RunID); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}
	if _, err := ReadRun(run.RunID); err == nil {
		t.Fatal("manifest should be gone after PurgeRun")
	}
	jobs, _ := ListJobs()
	for _, j := range jobs {
		if j.RunID == run.RunID {
			t.Fatalf("job %s for the purged run should be gone", j.JobID)
		}
	}
}
