package subagent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWithRunLock_WiringAndInvariants: WithRunLock runs fn under a flock on runs/<id>.lock, propagates
// fn's error, and a different id is independent. The cross-process MUTUAL EXCLUSION is flock's OS-level
// guarantee (the three config scopes rely on the same primitive; the race detector can't observe it, and
// the single-threaded board never contends in-process). The load-bearing invariant tested here is that
// `.lock` is NOT a GC'd sidecar — GC must never unlink a possibly-held flock (unlink+recreate = new inode
// = lost exclusion).
func TestWithRunLock_WiringAndInvariants(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const runID = "33333333-3333-3333-3333-333333333333"
	ran := false
	if err := WithRunLock(runID, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("WithRunLock: %v", err)
	}
	if !ran {
		t.Fatal("WithRunLock must run fn")
	}
	lockPath, err := RunLockPath(runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(lockPath); serr != nil {
		t.Fatalf("the lock file runs/<id>.lock should exist after WithRunLock: %v", serr)
	}
	for _, ext := range runSidecarExts {
		if ext == ".lock" {
			t.Fatal(".lock must NOT be in runSidecarExts — GC unlinking a held flock would break mutual exclusion")
		}
	}
	sentinel := errors.New("boom")
	if werr := WithRunLock(runID, func() error { return sentinel }); werr != sentinel {
		t.Fatalf("WithRunLock must propagate fn's error, got %v", werr)
	}
	if werr := WithRunLock("44444444-4444-4444-4444-444444444444", func() error { return nil }); werr != nil {
		t.Fatalf("a different run id must be independent: %v", werr)
	}
}

// TestFinalizeRunLeaves: finalizeRunLeaves writes a terminal failure cache for every leaf of the run that
// the dead engine left without a result, and touches nothing else — not a finished leaf (already cached),
// not another run's leaf.
func TestFinalizeRunLeaves(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const runID = "11111111-1111-1111-1111-111111111111"
	write := func(jobID, rid, status string, withResult bool) {
		meta := jobMeta{JobID: jobID, RunID: rid, Provider: "glm", Model: "m", Status: status,
			StartedAt: time.Now().Format(time.RFC3339)}
		data, _ := json.Marshal(meta)
		if werr := os.WriteFile(filepath.Join(dir, jobID+".json"), data, 0o600); werr != nil {
			t.Fatal(werr)
		}
		if withResult {
			r, _ := json.Marshal(Result{OK: true, JobID: jobID, Status: "done"})
			_ = os.WriteFile(filepath.Join(dir, jobID+".result.json"), r, 0o600)
		}
	}
	write("ghost", runID, "running", false)                                  // mid-flight leaf of this run
	write("finished", runID, "done", true)                                   // already cached
	write("other", "22222222-2222-2222-2222-222222222222", "running", false) // a different run

	finalizeRunLeaves(runID)

	if _, err := os.Stat(filepath.Join(dir, "ghost.result.json")); err != nil {
		t.Fatalf("the run's mid-flight leaf must be finalized with a result cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.result.json")); !os.IsNotExist(err) {
		t.Fatalf("a different run's leaf must NOT be finalized")
	}
}
