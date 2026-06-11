package subagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/pinned"
)

// mintHeldFixture mints a queued workflow leaf and parks it held.
func mintHeldFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	jobID := MintQueuedLeaf(Request{Provider: "v", RunID: "run-h", Phase: "p", Label: "l", LeadSessionID: "sess"}, "m")
	if jobID == "" {
		t.Fatal("mint failed")
	}
	HoldLeaf(jobID)
	return jobID
}

// TestHoldLeaf_NonTerminalEverywhere: a held leaf classifies "held" (not queued, not a
// terminal state), survives GC and clear-finished, and carries its grouping fields.
func TestHoldLeaf_NonTerminalEverywhere(t *testing.T) {
	jobID := mintHeldFixture(t)
	res := StatusFor(jobID)
	if res.Status != "held" || !res.OK {
		t.Fatalf("StatusFor = %q (OK=%v), want held/OK", res.Status, res.OK)
	}
	if res.RunID != "run-h" || res.Phase != "p" {
		t.Errorf("held result lost grouping fields: %+v", res)
	}
	if r := GC(0); !r.OK {
		t.Fatalf("gc: %+v", r)
	}
	if res := StatusFor(jobID); res.Status != "held" {
		t.Errorf("after GC --older-than 0: status = %q, want held (GC must treat held as live)", res.Status)
	}
	if _, err := ClearFinished("sess", pinned.Set{}); err != nil {
		t.Fatalf("clear-finished: %v", err)
	}
	if res := StatusFor(jobID); res.Status != "held" {
		t.Errorf("after clear-finished: status = %q, want held (held is not terminal)", res.Status)
	}
}

// TestHoldSuppressesStoppedFinalize: the killed attempt's stopped-class finalize writes NO
// terminal cache for a held leaf — no window exists where GC could read the job as finished.
func TestHoldSuppressesStoppedFinalize(t *testing.T) {
	jobID := mintHeldFixture(t)
	FinalizeQueuedLeafFailed(jobID, Result{ErrorCode: ErrCodeStopped, ErrorMsg: "leaf stop"})
	dir, _ := jobsDir()
	if _, err := os.Stat(filepath.Join(dir, jobID+".result.json")); err == nil {
		t.Fatal("a held leaf's stopped-class finalize must be suppressed (no result cache)")
	}
	if res := StatusFor(jobID); res.Status != "held" {
		t.Errorf("status = %q, want held", res.Status)
	}
}

// TestHoldNormalizedOnSuccessWins: an OK result that beat the kill writes its done cache
// AND normalizes the held meta — no stale held meta survives a settle.
func TestHoldNormalizedOnSuccessWins(t *testing.T) {
	jobID := mintHeldFixture(t)
	finalizeSyncJob(jobID, Result{OK: true, NumTurns: 1})
	res := StatusFor(jobID)
	if res.Status != "done" {
		t.Fatalf("status = %q, want done (success-wins writes the cache)", res.Status)
	}
	dir, _ := jobsDir()
	raw, err := os.ReadFile(filepath.Join(dir, jobID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Status != "done" {
		t.Errorf("meta status = %q, want done (normalized to the cache)", meta.Status)
	}
}

// TestNormalizeHeldLeaf: a pre-mark that lost its race (the directive landed after the
// terminal cache) is cleared to the cache's status; with no cache it is left alone.
func TestNormalizeHeldLeaf(t *testing.T) {
	jobID := mintHeldFixture(t)
	NormalizeHeldLeaf(jobID) // no cache → no-op
	if res := StatusFor(jobID); res.Status != "held" {
		t.Fatalf("status = %q, want held (no cache to restore from)", res.Status)
	}
	dir, _ := jobsDir()
	if err := os.WriteFile(filepath.Join(dir, jobID+".result.json"), []byte(`{"ok":true,"status":"done"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	NormalizeHeldLeaf(jobID)
	raw, err := os.ReadFile(filepath.Join(dir, jobID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Status != "done" {
		t.Errorf("meta status = %q, want done (normalized to the cache)", meta.Status)
	}
}

// TestFinalizeRunLeavesReleasesHold: the external whole-run stop terminal-stops a held
// member — the hold suppression must not survive the run's death.
func TestFinalizeRunLeavesReleasesHold(t *testing.T) {
	jobID := mintHeldFixture(t)
	finalizeRunLeaves("run-h")
	if res := StatusFor(jobID); res.Status != "stopped" {
		t.Errorf("held leaf after finalizeRunLeaves = %q, want stopped", res.Status)
	}
}

// TestRequeueLeaf: restart flips a held leaf back to a queued placeholder at the next
// attempt, with the terminal sidecars dropped.
func TestRequeueLeaf(t *testing.T) {
	jobID := mintHeldFixture(t)
	dir, _ := jobsDir()
	if err := os.WriteFile(filepath.Join(dir, jobID+".answer"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	RequeueLeaf(jobID, 2)
	res := StatusFor(jobID)
	if res.Status != "queued" {
		t.Fatalf("status = %q, want queued", res.Status)
	}
	if res.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", res.Attempt)
	}
	if _, err := os.Stat(filepath.Join(dir, jobID+".answer")); err == nil {
		t.Error("stale answer sidecar should be dropped on requeue")
	}
}

// TestRegisterAfterHold_PreservesHold: a registration racing in AFTER the engine's
// kill-and-HOLD pre-mark must not clobber the held meta back to running — it returns
// registerHeld so the attempt exits without ever finalizing.
func TestRegisterAfterHold_PreservesHold(t *testing.T) {
	jobID := mintHeldFixture(t)
	if got := registerSyncJob(jobID, Request{Provider: "v", RunID: "run-h"}, "m", "", "", 0); got != registerHeld {
		t.Fatalf("registerSyncJob on a held meta = %v, want registerHeld", got)
	}
	if res := StatusFor(jobID); res.Status != "held" {
		t.Fatalf("status = %q, want held (register must not clobber the pre-mark)", res.Status)
	}
	dir, _ := jobsDir()
	if _, err := os.Stat(filepath.Join(dir, jobID+".result.json")); err == nil {
		t.Fatal("no terminal cache may exist for a held leaf")
	}
}

// TestHoldAfterSettle_NoOps: a directive landing after the attempt already cached a
// terminal result must not flip the settled job to held (success-beats-kill stays
// authoritative; no terminal cache may sit under a held meta).
func TestHoldAfterSettle_NoOps(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	jobID := MintQueuedLeaf(Request{Provider: "v", RunID: "run-s"}, "m")
	if jobID == "" {
		t.Fatal("mint failed")
	}
	if got := registerSyncJob(jobID, Request{Provider: "v", RunID: "run-s"}, "m", "", "", 0); got != registerOK {
		t.Fatalf("registerSyncJob = %v, want registerOK", got)
	}
	finalizeSyncJob(jobID, Result{OK: true, NumTurns: 1})
	HoldLeaf(jobID)
	if res := StatusFor(jobID); res.Status != "done" {
		t.Fatalf("status = %q, want done (a settled job must not be re-held)", res.Status)
	}
}

// TestHoldVsFinalize_NeverTerminalCacheUnderHold races the engine's pre-mark against
// the leaf's finalize: whichever order the mutex serializes, a held meta and a terminal
// cache must never coexist.
func TestHoldVsFinalize_NeverTerminalCacheUnderHold(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		jobID := MintQueuedLeaf(Request{Provider: "v", RunID: "run-r"}, "m")
		if jobID == "" {
			t.Fatal("mint failed")
		}
		if got := registerSyncJob(jobID, Request{Provider: "v", RunID: "run-r"}, "m", "", "", 0); got != registerOK {
			t.Fatalf("registerSyncJob = %v, want registerOK", got)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); HoldLeaf(jobID) }()
		go func() {
			defer wg.Done()
			finalizeSyncJob(jobID, Result{ErrorCode: ErrCodeStopped, ErrorMsg: "kill"})
		}()
		wg.Wait()
		meta, merr := readMeta(dir, jobID)
		if merr != nil {
			t.Fatal(merr)
		}
		_, cerr := os.Stat(filepath.Join(dir, jobID+".result.json"))
		if meta.Status == "held" && cerr == nil {
			t.Fatal("terminal cache exists under a held meta")
		}
	}
}
