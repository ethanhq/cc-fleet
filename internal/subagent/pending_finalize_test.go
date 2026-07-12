package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReapedFinalizeClearsPendingIdentity: a sync member whose cmd.Start FAILED (onStart never ran,
// so ChildIdentityPending stays set with no ChildPID) finalizes through the REAPED path — runClaude
// returned, so the child is provably not running. That terminal write must clear the pending stamp AND
// the engine-proxy PID, so the leaf reads process-free and the keep-rule no longer force-keeps it. The
// SYNTHETIC finalize (an external reclaimer that can't prove the child dead) must instead PRESERVE the
// stamp — a crashed engine's orphan may still be live.
func TestReapedFinalizeClearsPendingIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}

	// The pending shape registerSyncJob leaves before recordChildIdentity: engine-proxy PID, RunID set,
	// pending true, no ChildPID.
	pending := func(jobID string) jobMeta {
		return jobMeta{JobID: jobID, RunID: "r", PID: deadEnginePID, ChildIdentityPending: true}
	}

	t.Run("reaped finalize clears pending + PID", func(t *testing.T) {
		writeLeafMeta(t, pending("reaped"), false)
		finalizeSyncJobReaped("reaped", fail(ErrCodeFailed, "spawn failed", "glm", ""))

		m, merr := readMeta(dir, "reaped")
		if merr != nil {
			t.Fatalf("readMeta: %v", merr)
		}
		if m.ChildIdentityPending {
			t.Error("a reaped finalize must clear ChildIdentityPending (child provably not running)")
		}
		if m.PID != 0 {
			t.Errorf("a reaped finalize must zero the engine-proxy PID, got %d", m.PID)
		}
		if _, serr := os.Stat(filepath.Join(dir, "reaped.result.json")); serr != nil {
			t.Errorf("the terminal result cache must be written, err=%v", serr)
		}
		if leafProcessMayBeAlive(m) {
			t.Error("a cleared cmd.Start-fails leaf must read process-free")
		}
		if isLiveOrPendingOrphanEvidence(m) {
			t.Error("the keep-rule must no longer force-keep a cleared cmd.Start-fails leaf")
		}
	})

	t.Run("synthetic finalize preserves pending (crash veto stands)", func(t *testing.T) {
		writeLeafMeta(t, pending("synthetic"), false)
		finalizeSyncJob("synthetic", fail(ErrCodeStopped, "run stopped before the leaf finished", "glm", ""))

		m, merr := readMeta(dir, "synthetic")
		if merr != nil {
			t.Fatalf("readMeta: %v", merr)
		}
		if !m.ChildIdentityPending || m.PID != deadEnginePID {
			t.Errorf("a synthetic finalize must preserve the pending stamp + PID, got pending=%v PID=%d", m.ChildIdentityPending, m.PID)
		}
		if !leafProcessMayBeAlive(m) || !isLiveOrPendingOrphanEvidence(m) {
			t.Error("a synthetic-finalized pending member must keep its read-side veto and force-keep evidence")
		}
	})

	// A HELD + pending member whose reaped finalize maps to `stopped` (the hold cancelled the attempt):
	// the terminal cache is SUPPRESSED (no cache under a live hold), but the in-process reap still clears
	// the false-pending stamp + PID so it can't force-keep the row forever once the hold is released.
	t.Run("held+stopped reaped finalize clears pending, still suppresses cache", func(t *testing.T) {
		m := pending("heldstop")
		m.Status = "held"
		m.PID = 0 // a hold premark clears PID
		writeLeafMeta(t, m, false)
		finalizeSyncJobReaped("heldstop", fail(ErrCodeStopped, "leaf held while the attempt was stopping", "glm", ""))

		got, merr := readMeta(dir, "heldstop")
		if merr != nil {
			t.Fatalf("readMeta: %v", merr)
		}
		if got.ChildIdentityPending {
			t.Error("a held+stopped reaped finalize must still clear the false-pending stamp")
		}
		if got.Status != "held" {
			t.Errorf("the row must stay held (a live frame awaiting restart), got %q", got.Status)
		}
		if _, serr := os.Stat(filepath.Join(dir, "heldstop.result.json")); serr == nil {
			t.Error("a hold must still SUPPRESS the terminal result cache")
		}
		if isLiveOrPendingOrphanEvidence(got) {
			t.Error("the keep-rule must no longer force-keep the held row via the pending clause")
		}
	})
}

// TestReapedFinalizeUnblocksWorktreeReclaim: end-to-end, a dead-engine worktree run with a
// cmd.Start-fails member strands until the member's veto is cleared. After the REAPED finalize the
// member reads process-free → runLeafScan finds no live leaf → a path-safe PurgeRun proceeds and drops
// the workdir. The genuine-crash shape (pending, never reaped-finalized) keeps its veto → PurgeRun
// refuses. Disabling the finalize clear-site reverts the first case to a refusal (the bite check).
func TestReapedFinalizeUnblocksWorktreeReclaim(t *testing.T) {
	origPS := procStartFn
	procStartFn = func(int) (string, bool) { return "tok", true }
	t.Cleanup(func() { procStartFn = origPS })

	t.Run("reaped finalize → PurgeRun proceeds", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "pend-reaped-run" // path-safe
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID})
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: deadEnginePID, ChildIdentityPending: true}, false)

		// The reaped finalize (cmd.Start failed) clears the stamp; the leaf now reads process-free.
		finalizeSyncJobReaped("leaf1", fail(ErrCodeFailed, "spawn failed", "glm", ""))
		if live, ok := runLeafScan(); !ok || live[runID] {
			t.Fatalf("runLeafScan must report no live leaf after the reaped finalize, live=%v ok=%v", live, ok)
		}

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun must proceed after the reaped finalize, got %v", err)
		}
		if _, serr := os.Stat(wtDir); !os.IsNotExist(serr) {
			t.Errorf("the workdir must be removed once the cmd.Start-fails veto is cleared, err=%v", serr)
		}
	})

	t.Run("genuine crash (never finalized) → PurgeRun refuses", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "pend-crash-run" // path-safe
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID})
		writeLeafMeta(t, jobMeta{JobID: "leaf1", RunID: runID, PID: deadEnginePID, ChildIdentityPending: true}, false)

		if err := PurgeRun(runID); err == nil {
			t.Error("PurgeRun must refuse while a genuine-crash pending member vetoes the segment")
		}
		if _, serr := os.Stat(wtDir); serr != nil {
			t.Errorf("the refused PurgeRun must leave the workdir, err=%v", serr)
		}
	})
}
