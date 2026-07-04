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

	// FOREGROUND identity path (EnginePID 0): the run is provably dead iff its recorded fg engine is.
	origPS := procStartFn
	t.Cleanup(func() { procStartFn = origPS })
	procStartFn = func(int) (string, bool) { return "fg-tok", true }
	// A Ctrl-C'd / crashed fg run — recorded fg pid now DEAD → provably dead.
	if !RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "stopped", EnginePID: 0, FgEnginePID: dead, FgEngineProcStart: "fg-tok"}) {
		t.Error("a stopped fg run with a dead fg pid must be provably dead")
	}
	if !RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "running", EnginePID: 0, FgEnginePID: dead}) {
		t.Error("a crashed fg run (running + dead fg pid) must be provably dead")
	}
	// A still-live fg engine (pid alive + token matches) → NOT provably dead.
	if RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: "fg-tok"}) {
		t.Error("a blind-stopped LIVE fg engine must not read as provably dead")
	}
	// A recycled fg pid (alive + token MISMATCH) → provably dead.
	if !RunEngineProvablyNotLive(WorkflowRun{RunID: "r1", Status: "stopped", EnginePID: 0, FgEnginePID: self, FgEngineProcStart: "stale"}) {
		t.Error("a recycled fg pid (token mismatch) must be provably dead")
	}
}

// TestClassifyDetachedEngine drives the DETACHED tri-state via the reuseGuardArgv + procStartFn seams:
// no pid → Unknown; dead pid → Dead; alive+argv-match → Live; alive+argv-mismatch → Dead (recycled);
// alive+argv-unreadable+token-match → Live, +token-mismatch → Dead; alive+argv-unreadable+token-UNREADABLE
// → Unknown (a binary !EngineAlive would wrongly call this dead); alive+nothing-recorded → Unknown.
func TestClassifyDetachedEngine(t *testing.T) {
	origArgv := reuseGuardArgv
	origPS := procStartFn
	t.Cleanup(func() { reuseGuardArgv = origArgv; procStartFn = origPS })
	self := os.Getpid()
	const dead = 0x7ffffffe
	engineArgv := []string{"cc-fleet", "workflow", "run", "--run-id", "r1", "s.js"}
	otherArgv := []string{"some", "other", "proc"}
	argvOK := func(a []string) func(int) ([]string, bool) { return func(int) ([]string, bool) { return a, true } }
	argvFail := func(int) ([]string, bool) { return nil, false }
	tokOK := func(s string) func(int) (string, bool) { return func(int) (string, bool) { return s, true } }
	tokFail := func(int) (string, bool) { return "", false }

	cases := []struct {
		name  string
		run   WorkflowRun
		argv  func(int) ([]string, bool)
		token func(int) (string, bool)
		want  DetachedLiveness
	}{
		{"no pid", WorkflowRun{RunID: "r1", EnginePID: 0}, argvFail, tokFail, DetachedUnknown},
		{"dead pid", WorkflowRun{RunID: "r1", EnginePID: dead}, argvOK(engineArgv), tokOK("t"), DetachedDead},
		{"alive + argv match", WorkflowRun{RunID: "r1", EnginePID: self}, argvOK(engineArgv), tokFail, DetachedLive},
		{"alive + argv mismatch (recycled)", WorkflowRun{RunID: "r1", EnginePID: self}, argvOK(otherArgv), tokOK("t"), DetachedDead},
		{"alive + argv unreadable + token match", WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "tok"}, argvFail, tokOK("tok"), DetachedLive},
		{"alive + argv unreadable + token mismatch", WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "old"}, argvFail, tokOK("new"), DetachedDead},
		{"alive + argv and token unreadable", WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "tok"}, argvFail, tokFail, DetachedUnknown},
		{"alive + nothing recorded (pre-token)", WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: ""}, argvFail, tokOK("t"), DetachedUnknown},
	}
	for _, c := range cases {
		reuseGuardArgv = c.argv
		procStartFn = c.token
		if got := ClassifyDetachedEngine(c.run); got != c.want {
			t.Errorf("%s: ClassifyDetachedEngine = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestClassifyFgEngine drives the foreground liveness classifier directly via the procStartFn seam:
// no fg pid → Unknown; dead pid → Dead; alive+token-match → Alive; alive+token-mismatch → Dead
// (recycled); alive+token-unreadable or unrecorded → Unknown (fail-safe). No argv is ever consulted.
func TestClassifyFgEngine(t *testing.T) {
	orig := procStartFn
	t.Cleanup(func() { procStartFn = orig })
	self := os.Getpid()
	const dead = 0x7ffffffe

	cases := []struct {
		name  string
		run   WorkflowRun
		token func(int) (string, bool)
		want  FgLiveness
	}{
		{"no fg pid", WorkflowRun{FgEnginePID: 0}, func(int) (string, bool) { return "t", true }, FgUnknown},
		{"dead pid", WorkflowRun{FgEnginePID: dead, FgEngineProcStart: "t"}, func(int) (string, bool) { return "t", true }, FgDead},
		{"alive + token match", WorkflowRun{FgEnginePID: self, FgEngineProcStart: "t"}, func(int) (string, bool) { return "t", true }, FgAlive},
		{"alive + token mismatch (recycled)", WorkflowRun{FgEnginePID: self, FgEngineProcStart: "old"}, func(int) (string, bool) { return "new", true }, FgDead},
		{"alive + token unreadable", WorkflowRun{FgEnginePID: self, FgEngineProcStart: "t"}, func(int) (string, bool) { return "", false }, FgUnknown},
		{"alive + no recorded token", WorkflowRun{FgEnginePID: self, FgEngineProcStart: ""}, func(int) (string, bool) { return "t", true }, FgUnknown},
	}
	for _, c := range cases {
		procStartFn = c.token
		if got := ClassifyFgEngine(c.run); got != c.want {
			t.Errorf("%s: ClassifyFgEngine = %v, want %v", c.name, got, c.want)
		}
	}
}
