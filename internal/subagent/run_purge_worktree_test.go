package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

// seedWorktreeTemp creates a cc-fleet-worktrees/<runID> temp tree (mirroring a run's
// isolation-worktree workdir root), registers its cleanup, and returns its path.
func seedWorktreeTemp(t *testing.T, runID string) string {
	t.Helper()
	wtDir := filepath.Join(os.TempDir(), "cc-fleet-worktrees", runID)
	if err := os.MkdirAll(filepath.Join(wtDir, "x"), 0o700); err != nil {
		t.Fatalf("mkdir worktree temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(wtDir) })
	return wtDir
}

const deadEnginePID = 0x7ffffffe // never a live process in the test env → provably dead

// TestPurgeRunWorktreeTempGating: PurgeRun drops a run's isolation-worktree temp workdirs ONLY
// when the run is provably dead — a recorded detached pid that is dead. A crashed detached run,
// and a detached run taken down via the real StopRun path (dead pid + start token retained as the
// death evidence), are purged. A running FOREGROUND run, a FOREGROUND run blind-flipped to
// "stopped" by StopRun (EnginePID 0 — nothing reaped, engine may still be live), and an ABSENT
// record are all left intact — the workdir may be a live leaf's cwd, and it is not recreate-safe.
func TestPurgeRunWorktreeTempGating(t *testing.T) {
	t.Run("crashed detached (running, dead pid) → removed", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "run-purge-crashed"
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: deadEnginePID})

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
			t.Errorf("crashed-detached run's worktree temp dir should be gone (err=%v)", err)
		}
	})

	t.Run("detached stopped via real StopRun → provably dead → removed", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "run-purge-stopped"
		wtDir := seedWorktreeTemp(t, runID)
		// A detached run whose engine pid is already dead: StopRun reaps nothing to kill, flips it
		// to "stopped", and RETAINS the dead pid as the death evidence.
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: deadEnginePID})
		stopped, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun: %v", err)
		}
		if stopped.EnginePID != deadEnginePID {
			t.Errorf("StopRun must retain the dead detached pid as death evidence, got EnginePID=%d", stopped.EnginePID)
		}
		if !RunEngineProvablyNotLive(stopped) {
			t.Error("a detached run stopped via StopRun must remain provably dead (pid retained + dead)")
		}

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
			t.Errorf("stopped-detached run's worktree temp dir should be gone (err=%v)", err)
		}
	})

	t.Run("foreground flipped to stopped via real StopRun → survives", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "run-purge-fg-stopped"
		wtDir := seedWorktreeTemp(t, runID)
		// A live FOREGROUND run records EnginePID 0. `workflow stop` blind-flips it to "stopped"
		// while reaping nothing — the engine may still be writing, so it must NOT read as dead.
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0})
		stopped, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun: %v", err)
		}
		if RunEngineProvablyNotLive(stopped) {
			t.Error("a foreground run blind-flipped to stopped (EnginePID 0) must NOT read as provably dead")
		}

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); err != nil {
			t.Errorf("foreground-stopped run's worktree temp dir must survive the purge (err=%v)", err)
		}
	})

	t.Run("running foreground (EnginePID 0) → survives", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "run-purge-fg"
		wtDir := seedWorktreeTemp(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0})

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); err != nil {
			t.Errorf("running-foreground run's worktree temp dir must survive the purge (err=%v)", err)
		}
	})

	t.Run("absent record → survives (fail closed)", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "run-purge-absent"
		wtDir := seedWorktreeTemp(t, runID)
		// No manifest: PurgeRun's provable-death re-read fails → fail closed → temp left intact.

		if err := PurgeRun(runID); err != nil {
			t.Fatalf("PurgeRun: %v", err)
		}
		if _, err := os.Stat(wtDir); err != nil {
			t.Errorf("absent-record run's worktree temp dir must survive the purge (err=%v)", err)
		}
	})
}
