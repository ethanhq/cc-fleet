package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStopRunTriState drives StopRun's DetachedLive / DetachedDead / DetachedUnknown ladder and the
// wedged-terminal escape hatch via the classification seams (reuseGuardArgv / procStartFn) + the reap
// seam (reapEngineTreeFn — a no-op so EnginePID==self is never actually killed). The key regression: a
// retained detached pid ALIVE but unverifiable must be REFUSED (status + journal untouched), NOT
// finalized-and-flipped-stopped under a possibly-live engine.
func TestStopRunTriState(t *testing.T) {
	self := os.Getpid()
	engineArgv := func(runID string) []string {
		return []string{"cc-fleet", "workflow", "run", "--run-id", runID, "s.js"}
	}
	otherArgv := []string{"some", "other", "proc"}

	// resultCached reports whether a member leaf got a synthetic terminal (finalizeRunLeaves ran).
	resultCached := func(t *testing.T, jobID string) bool {
		t.Helper()
		dir, _ := jobsDir()
		_, err := os.Stat(filepath.Join(dir, jobID+".result.json"))
		return err == nil
	}

	t.Run("running + live-unverifiable → refused, status+leaf untouched", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origArgv, origPS, origReap := reuseGuardArgv, procStartFn, reapEngineTreeFn
		reaped := false
		reuseGuardArgv = func(int) ([]string, bool) { return nil, false } // argv unreadable
		procStartFn = func(int) (string, bool) { return "", false }       // token unreadable → Unknown
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reuseGuardArgv, procStartFn, reapEngineTreeFn = origArgv, origPS, origReap })

		const runID = "st-unverif"
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self, EngineProcStart: "tok"})
		writeLeafMeta(t, jobMeta{JobID: "lf", RunID: runID, PID: self, Status: "running"}, false)

		_, err := StopRun(runID)
		if err == nil || !strings.Contains(err.Error(), "unverifiable") {
			t.Fatalf("StopRun on a live-unverifiable engine must refuse, got %v", err)
		}
		if reaped {
			t.Error("an unverifiable pid must never be reaped")
		}
		if run, _ := ReadRun(runID); run.Status != "running" {
			t.Errorf("status must be left untouched (not flipped stopped), got %q", run.Status)
		}
		if resultCached(t, "lf") {
			t.Error("no leaf may be finalized under a possibly-live engine")
		}
	})

	t.Run("running + dead detached → finalize ghosts + stop", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origReap := reapEngineTreeFn
		reaped := false
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reapEngineTreeFn = origReap })

		const runID = "st-dead"
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: deadEnginePID})
		writeLeafMeta(t, jobMeta{JobID: "lf", RunID: runID, PID: deadEnginePID, Status: "running"}, false)

		run, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun on a dead detached engine must succeed, got %v", err)
		}
		if run.Status != "stopped" {
			t.Errorf("a dead detached run must flip stopped, got %q", run.Status)
		}
		if reaped {
			t.Error("a dead pid must not be reaped")
		}
		if !resultCached(t, "lf") {
			t.Error("a dead detached run's ghost leaf must be finalized")
		}
	})

	t.Run("running foreground (EnginePID 0) → blind-flip stopped", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origReap := reapEngineTreeFn
		reaped := false
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reapEngineTreeFn = origReap })

		const runID = "st-fg"
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0})

		run, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun on a foreground run must succeed, got %v", err)
		}
		if run.Status != "stopped" || reaped {
			t.Errorf("a foreground run flips stopped with no reap, got status=%q reaped=%v", run.Status, reaped)
		}
	})

	// The DetachedLive reap path (running + terminal escape hatch): reuseGuardArgv reads the engine argv
	// as a MATCH before the reap and a MISMATCH after (via the reaped flag), so the pid is Live going in
	// and reads Dead as WaitEngineStopped polls — deterministic, no real process killed.
	liveThenDead := func(t *testing.T, runID string) *bool {
		t.Helper()
		origArgv, origReap := reuseGuardArgv, reapEngineTreeFn
		reaped := false
		reuseGuardArgv = func(int) ([]string, bool) {
			if reaped {
				return otherArgv, true // post-reap: readable mismatch → Dead
			}
			return engineArgv(runID), true // pre-reap: match → Live
		}
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reuseGuardArgv, reapEngineTreeFn = origArgv, origReap })
		return &reaped
	}

	t.Run("running + verifiably-live → reap + confirm dead + stop", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "st-live"
		reaped := liveThenDead(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self})
		writeLeafMeta(t, jobMeta{JobID: "lf", RunID: runID, PID: self, Status: "running"}, false)

		run, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun on a verifiably-live engine must reap + stop, got %v", err)
		}
		if !*reaped || run.Status != "stopped" || !resultCached(t, "lf") {
			t.Errorf("live engine must be reaped + run stopped + leaf finalized, got reaped=%v status=%q cached=%v", *reaped, run.Status, resultCached(t, "lf"))
		}
	})

	t.Run("wedged terminal + Unknown pid → refused (not a no-op)", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origArgv, origPS, origReap := reuseGuardArgv, procStartFn, reapEngineTreeFn
		reaped := false
		reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
		procStartFn = func(int) (string, bool) { return "", false }
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reuseGuardArgv, procStartFn, reapEngineTreeFn = origArgv, origPS, origReap })

		const runID = "st-wedge-unk"
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: self, EngineProcStart: "tok"})

		_, err := StopRun(runID)
		if err == nil || !strings.Contains(err.Error(), "unverifiable") {
			t.Fatalf("a wedged terminal record with an Unknown live pid must fail closed, got %v", err)
		}
		if reaped {
			t.Error("an unverifiable pid must never be reaped")
		}
	})

	t.Run("wedged terminal + Live pid → reaps (escape hatch)", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		const runID = "st-wedge-live"
		reaped := liveThenDead(t, runID)
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: self})

		run, err := StopRun(runID)
		if err != nil {
			t.Fatalf("the escape hatch must reap a wedged-terminal live engine, got %v", err)
		}
		if !*reaped || run.Status != "stopped" {
			t.Errorf("a wedged-terminal live engine must be reaped (not no-oped), got reaped=%v status=%q", *reaped, run.Status)
		}
	})

	t.Run("terminal + dead pid → no-op", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		origReap := reapEngineTreeFn
		reaped := false
		reapEngineTreeFn = func(int) { reaped = true }
		t.Cleanup(func() { reapEngineTreeFn = origReap })

		const runID = "st-term-dead"
		writeRunForTest(t, WorkflowRun{RunID: runID, StartedAt: "2026-01-01T00:00:00Z", Status: "done", EnginePID: deadEnginePID})
		writeLeafMeta(t, jobMeta{JobID: "lf", RunID: runID, PID: deadEnginePID, Status: "running"}, false)

		run, err := StopRun(runID)
		if err != nil {
			t.Fatalf("StopRun on a normal terminal record must no-op, got %v", err)
		}
		if run.Status != "done" || reaped || resultCached(t, "lf") {
			t.Errorf("a terminal + dead-pid record must be returned untouched, got status=%q reaped=%v cached=%v", run.Status, reaped, resultCached(t, "lf"))
		}
	})
}
