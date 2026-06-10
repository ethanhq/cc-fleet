package subagent

import (
	"os"
	"testing"
)

// TestEngineAlive covers the liveness check a watcher uses to stop waiting on a stale "running"
// manifest: a dead/foreground pid is gone; a live pid is alive only if its argv still proves it
// is this run's engine (so a RECYCLED pid reads as gone), EXCEPT where argv is unavailable
// (Windows-like) — there it fails SOFT to alive so a live engine is never falsely declared gone.
func TestEngineAlive(t *testing.T) {
	orig := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = orig })
	self := os.Getpid()
	const dead = 0x7ffffffe // a pid that is (effectively) not running
	engineArgv := []string{"cc-fleet", "workflow", "run", "--run-id", "r1", "s.js"}

	// Foreground run (EnginePID 0) is never "alive".
	if EngineAlive(WorkflowRun{EnginePID: 0, RunID: "r1"}) {
		t.Error("a foreground run (EnginePID 0) must read as not-alive")
	}

	// A dead pid is gone — argv is not even consulted (would match here if it were).
	reuseGuardArgv = func(int) ([]string, bool) { return engineArgv, true }
	if EngineAlive(WorkflowRun{EnginePID: dead, RunID: "r1"}) {
		t.Error("a dead pid must read as gone")
	}

	// Alive pid whose argv is still this run's engine → alive.
	reuseGuardArgv = func(int) ([]string, bool) { return engineArgv, true }
	if !EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1"}) {
		t.Error("a live engine with matching argv must read as alive")
	}

	// Alive pid recycled to an unrelated process (argv mismatch) → gone (no indefinite hang).
	reuseGuardArgv = func(int) ([]string, bool) { return []string{"some", "other", "proc"}, true }
	if EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1"}) {
		t.Error("a recycled pid (argv no longer matches) must read as gone")
	}

	// Argv unavailable (platform without introspection) → fail soft to alive (no false gone).
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	if !EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1"}) {
		t.Error("argv-unavailable must fail soft to alive, never falsely declare a live engine gone")
	}
}
