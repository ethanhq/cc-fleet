//go:build !windows

package subagent

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestPurgeRunStopsLiveDetachedEngine (codex r20 correction): PurgeRun's one-action
// delete-a-running-run — a VERIFIABLY-live detached engine (DetachedLive) is stopped AND confirmed dead
// (reap tree + WaitEngineStopped) before the run is deleted. Drives a REAL killable child in its own
// process group as the "engine" pid, with a matching argv via the reuseGuardArgv seam, so the reap can
// never touch the test process.
func TestPurgeRunStopsLiveDetachedEngine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	child := exec.Command("sleep", "120")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // own group → reapEngineTree's -pid can't reach the test
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }() // reap the zombie promptly so pidAlive flips false after the kill
	t.Cleanup(func() { _ = child.Process.Kill(); <-done })
	pid := child.Process.Pid

	origArgv := reuseGuardArgv
	reuseGuardArgv = func(int) ([]string, bool) {
		return []string{"cc-fleet", "workflow", "run", "--run-id", "live-det", "s.js"}, true // matches → DetachedLive
	}
	t.Cleanup(func() { reuseGuardArgv = origArgv })

	run := WorkflowRun{RunID: "live-det", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: pid}
	if got := ClassifyDetachedEngine(run); got != DetachedLive {
		t.Fatalf("setup: expected DetachedLive, got %v", got)
	}
	if err := SaveRun(run); err != nil {
		t.Fatal(err)
	}

	if err := PurgeRun("live-det"); err != nil {
		t.Fatalf("PurgeRun must stop + delete a verifiably-live detached run, got %v", err)
	}
	if _, rerr := ReadRun("live-det"); rerr == nil {
		t.Error("PurgeRun must remove the run after stopping its engine")
	}
	if pidAlive(pid) {
		t.Error("PurgeRun must have reaped the live detached engine before deleting")
	}
}
