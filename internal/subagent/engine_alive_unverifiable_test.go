package subagent

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestEngineAliveUnverifiableFailsSoft (codex r30): EngineAlive must fail SOFT to alive on an
// alive-but-UNVERIFIABLE detached pid (argv unreadable AND its recorded token unreadable). The old code
// collapsed a token-read failure to false, so consumers that read !EngineAlive as death proof —
// critically WaitEngineStopped's poll after a reap — could succeed prematurely on a still-live engine.
func TestEngineAliveUnverifiableFailsSoft(t *testing.T) {
	self := os.Getpid()
	const dead = 0x7ffffffe
	origArgv, origPS := reuseGuardArgv, procStartFn
	t.Cleanup(func() { reuseGuardArgv, procStartFn = origArgv, origPS })

	// pidAlive + argv unreadable + a RECORDED token that the platform can't read → Unknown → ALIVE.
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	procStartFn = func(int) (string, bool) { return "", false }
	unverif := WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "tok"}
	if !EngineAlive(unverif) {
		t.Error("an alive-but-unverifiable pid (argv+token unreadable) must read ALIVE, never a false gone")
	}

	// The recycled/dead shapes still read gone (a READABLE argv mismatch, or a dead pid).
	reuseGuardArgv = func(int) ([]string, bool) { return []string{"other", "proc"}, true }
	if EngineAlive(WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "tok"}) {
		t.Error("a readable argv mismatch (recycled) must read gone")
	}
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	if EngineAlive(WorkflowRun{RunID: "r1", EnginePID: dead, EngineProcStart: "tok"}) {
		t.Error("a dead pid must read gone")
	}
}

// TestWaitEngineStoppedFailClosedOnUnverifiable (codex r30): WaitEngineStopped must TIME OUT (false) on
// an alive-but-unverifiable engine — the fail-closed path — instead of succeeding as it did when
// EngineAlive collapsed the token failure to false; a genuinely-dead pid still passes immediately. And
// StopRun's DetachedLive reap path, when the pid won't verify dead, surfaces the did-not-stop error
// rather than finalizing/flipping under a possibly-live engine.
func TestWaitEngineStoppedFailClosedOnUnverifiable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	self := os.Getpid()
	origArgv, origPS, origReap, origTO := reuseGuardArgv, procStartFn, reapEngineTreeFn, stopReapTimeout
	stopReapTimeout = 150 * time.Millisecond // keep the fail-closed path fast
	t.Cleanup(func() {
		reuseGuardArgv, procStartFn, reapEngineTreeFn, stopReapTimeout = origArgv, origPS, origReap, origTO
	})

	// A genuinely-dead pid passes Wait immediately.
	writeRunForTest(t, WorkflowRun{RunID: "dead", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: deadEnginePID})
	if !WaitEngineStopped("dead", time.Second) {
		t.Error("WaitEngineStopped must return true immediately for a dead pid")
	}

	// An alive-but-unverifiable engine: Wait must TIME OUT (fail-closed), not falsely report stopped.
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	procStartFn = func(int) (string, bool) { return "", false }
	writeRunForTest(t, WorkflowRun{RunID: "unverif", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self, EngineProcStart: "tok"})
	if WaitEngineStopped("unverif", 150*time.Millisecond) {
		t.Error("WaitEngineStopped must fail closed (time out) on an alive-but-unverifiable engine")
	}

	// StopRun's DetachedLive reap that doesn't verify the pid dead surfaces the did-not-stop error.
	// reuseGuardArgv reads the engine argv as a MATCH (→ DetachedLive going in); the reap seam is a no-op,
	// so the pid stays Live/unverifiable and WaitEngineStopped times out → StopRun fails closed.
	reuseGuardArgv = func(int) ([]string, bool) {
		return []string{"cc-fleet", "workflow", "run", "--run-id", "live", "s.js"}, true
	}
	reapEngineTreeFn = func(int) {} // reap sends the signal but the engine won't die in time
	writeRunForTest(t, WorkflowRun{RunID: "live", StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: self})
	_, err := StopRun("live")
	if err == nil || !strings.Contains(err.Error(), "did not stop in time") {
		t.Fatalf("StopRun's Live reap must surface the did-not-stop error when the pid won't verify dead, got %v", err)
	}
	if run, _ := ReadRun("live"); run.Status != "running" {
		t.Errorf("a did-not-stop reap must NOT flip the run stopped (fail closed), got %q", run.Status)
	}
}
