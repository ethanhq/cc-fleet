package subagent

import (
	"os"
	"testing"
)

// TestRunEngineProvablyNotLive: the ONLY death proof is a recorded detached EnginePID that is
// positively dead or recycled (!EngineAlive). Terminal status ALONE is not proof — a live
// foreground run blind-flipped to "stopped" keeps EnginePID 0 — and a foreground/pre-stamp run
// (EnginePID 0) is never provably dead. A live matching engine (even under a terminal status) and
// an alive-but-unverifiable pid are both spared.
func TestRunEngineProvablyNotLive(t *testing.T) {
	orig := reuseGuardArgv
	t.Cleanup(func() { reuseGuardArgv = orig })
	self := os.Getpid()
	const dead = 0x7ffffffe
	engineArgv := []string{"cc-fleet", "workflow", "run", "--run-id", "r1", "s.js"}

	// A recorded detached pid that is DEAD → provably dead, whatever the status: a crash left it
	// "running", and a StopRun'd detached run left it "stopped" with its dead pid retained.
	for _, st := range []string{"running", "stopped", "failed", "done"} {
		reuseGuardArgv = func(int) ([]string, bool) { return engineArgv, true }
		if !RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: st, EnginePID: dead}) {
			t.Errorf("status %q with a dead detached pid must be provably dead", st)
		}
	}

	// Terminal status ALONE is not proof: a foreground run blind-flipped to a terminal state keeps
	// EnginePID 0, and its engine may still be live.
	for _, st := range []string{"stopped", "failed", "done"} {
		if RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: st, EnginePID: 0}) {
			t.Errorf("terminal status %q with EnginePID 0 must NOT be provably dead (terminal alone is not proof)", st)
		}
	}

	// A running foreground engine (EnginePID 0) keeps writing — not provably dead.
	if RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "running", EnginePID: 0}) {
		t.Error("a running foreground engine (EnginePID 0) must not read as provably dead")
	}

	// A live pid recycled to an unrelated process (argv mismatch) → provably dead.
	reuseGuardArgv = func(int) ([]string, bool) { return []string{"some", "other"}, true }
	if !RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "running", EnginePID: self}) {
		t.Error("a recycled detached pid must read as provably dead")
	}

	// A live pid still this run's engine → not provably dead, even under a terminal status.
	for _, st := range []string{"running", "stopped", "done"} {
		reuseGuardArgv = func(int) ([]string, bool) { return engineArgv, true }
		if RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: st, EnginePID: self}) {
			t.Errorf("status %q with a live matching engine must not read as provably dead", st)
		}
	}

	// A live pid whose identity can't be read (no argv, no token) → not provably dead (fail SAFE).
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	if RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "running", EnginePID: self}) {
		t.Error("an alive-but-unverifiable pid must not read as provably dead")
	}
}
