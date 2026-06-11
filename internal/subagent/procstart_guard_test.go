package subagent

import (
	"os"
	"testing"
)

// TestEngineProcStartMatches covers the start-token engine identity used where
// argv is unreadable: a matching token positively identifies the engine, a
// mismatch (recycled pid) rejects it, and an empty token never matches (those
// manifests keep the argv-only behavior).
func TestEngineProcStartMatches(t *testing.T) {
	orig := procStartFn
	t.Cleanup(func() { procStartFn = orig })
	self := os.Getpid()

	procStartFn = func(int) (string, bool) { return "tok-1", true }
	if !engineProcStartMatches(WorkflowRun{EnginePID: self, EngineProcStart: "tok-1"}) {
		t.Error("a matching start token must identify the engine")
	}
	if engineProcStartMatches(WorkflowRun{EnginePID: self, EngineProcStart: "tok-0"}) {
		t.Error("a mismatched start token (recycled pid) must not match")
	}
	if engineProcStartMatches(WorkflowRun{EnginePID: self, EngineProcStart: ""}) {
		t.Error("an empty recorded token must never match")
	}
	if engineProcStartMatches(WorkflowRun{EnginePID: 0, EngineProcStart: "tok-1"}) {
		t.Error("EnginePID<=0 must never match")
	}

	procStartFn = func(int) (string, bool) { return "", false }
	if engineProcStartMatches(WorkflowRun{EnginePID: self, EngineProcStart: "tok-1"}) {
		t.Error("an unreadable live token must not match")
	}
}

// TestEngineAlive_StartToken covers the argv-unavailable branch: a recorded
// token decides liveness (match → alive, mismatch → gone), and only a
// token-less manifest keeps the legacy fail-soft-to-alive behavior.
func TestEngineAlive_StartToken(t *testing.T) {
	origArgv, origStart := reuseGuardArgv, procStartFn
	t.Cleanup(func() { reuseGuardArgv, procStartFn = origArgv, origStart })
	self := os.Getpid()

	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	procStartFn = func(int) (string, bool) { return "tok-1", true }
	if !EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1", EngineProcStart: "tok-1"}) {
		t.Error("argv-unavailable with a matching token must read alive")
	}
	if EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1", EngineProcStart: "tok-0"}) {
		t.Error("argv-unavailable with a mismatched token (recycled pid) must read gone")
	}
	if !EngineAlive(WorkflowRun{EnginePID: self, RunID: "r1"}) {
		t.Error("argv-unavailable without a token must fail soft to alive")
	}
}

// TestProcessAlive_NoArgvIntrospection covers the job-side reuse guard where
// argv is unreadable: the recorded start token decides (match → alive,
// mismatch → dead), and a token-less meta trusts the bare liveness check.
func TestProcessAlive_NoArgvIntrospection(t *testing.T) {
	origHas, origStart := hasArgvIntrospection, procStartFn
	t.Cleanup(func() { hasArgvIntrospection, procStartFn = origHas, origStart })
	hasArgvIntrospection = false
	self := os.Getpid()

	procStartFn = func(int) (string, bool) { return "tok-1", true }
	if !processAlive(self, "", "tok-1") {
		t.Error("a matching token must read alive")
	}
	if processAlive(self, "", "tok-0") {
		t.Error("a mismatched token (recycled pid) must read dead")
	}
	if !processAlive(self, "", "") {
		t.Error("a token-less meta must trust the bare liveness check")
	}
	if processAlive(0x7ffffffe, "", "tok-1") {
		t.Error("a dead pid must read dead regardless of token")
	}
}

// TestProcessAlive_SyncToken covers the marker-less (sync) job on a platform
// WITH argv introspection: the recorded token still guards a recycled parent
// pid, since there is no --settings marker to bind argv to.
func TestProcessAlive_SyncToken(t *testing.T) {
	origHas, origStart := hasArgvIntrospection, procStartFn
	t.Cleanup(func() { hasArgvIntrospection, procStartFn = origHas, origStart })
	hasArgvIntrospection = true
	self := os.Getpid()

	procStartFn = func(int) (string, bool) { return "tok-1", true }
	if !processAlive(self, "", "tok-1") {
		t.Error("a matching sync token must read alive")
	}
	if processAlive(self, "", "tok-0") {
		t.Error("a mismatched sync token (recycled pid) must read dead")
	}
	if !processAlive(self, "", "") {
		t.Error("a token-less sync meta must trust the bare liveness check")
	}
}

// TestProcessAlive_GuardComposition pins how the two reuse guards compose for a
// background job (settings marker present): a token MISMATCH is decisive (the
// per-provider --settings value can collide-match a later claude job of the
// same provider), while a token MATCH is not sufficient alone (the darwin token
// is seconds-coarse) — a readable argv must still agree.
func TestProcessAlive_GuardComposition(t *testing.T) {
	origHas, origStart, origArgv := hasArgvIntrospection, procStartFn, reuseGuardArgv
	t.Cleanup(func() { hasArgvIntrospection, procStartFn, reuseGuardArgv = origHas, origStart, origArgv })
	hasArgvIntrospection = true
	self := os.Getpid()
	matchingArgv := func(int) ([]string, bool) {
		return []string{"/x/claude/cli.js", "--settings", "/p/minimax.json"}, true
	}
	foreignArgv := func(int) ([]string, bool) { return []string{"some", "other", "proc"}, true }

	reuseGuardArgv = matchingArgv
	procStartFn = func(int) (string, bool) { return "tok-0", true }
	if processAlive(self, "/p/minimax.json", "tok-1") {
		t.Error("a mismatched token must read dead even when argv matches")
	}
	procStartFn = func(int) (string, bool) { return "tok-1", true }
	if !processAlive(self, "/p/minimax.json", "tok-1") {
		t.Error("token match + argv match must read alive")
	}
	reuseGuardArgv = foreignArgv
	if processAlive(self, "/p/minimax.json", "tok-1") {
		t.Error("a token match must not override an argv mismatch (coarse-token collision)")
	}
	// Token unreadable → the argv guard alone decides.
	reuseGuardArgv = matchingArgv
	procStartFn = func(int) (string, bool) { return "", false }
	if !processAlive(self, "/p/minimax.json", "tok-1") {
		t.Error("an unreadable token must fall back to the (matching) argv guard")
	}
}

// TestEngineIdentityMatches pins StopRun's kill guard: readable argv alone
// decides (a token match must not override an argv mismatch); the token decides
// only where argv is unreadable.
func TestEngineIdentityMatches(t *testing.T) {
	origArgv, origStart := reuseGuardArgv, procStartFn
	t.Cleanup(func() { reuseGuardArgv, procStartFn = origArgv, origStart })
	self := os.Getpid()
	run := WorkflowRun{RunID: "r1", EnginePID: self, EngineProcStart: "tok-1"}
	engineArgv := []string{"cc-fleet", "workflow", "run", "--run-id", "r1", "s.js"}
	procStartFn = func(int) (string, bool) { return "tok-1", true }

	reuseGuardArgv = func(int) ([]string, bool) { return engineArgv, true }
	if !engineIdentityMatches(run) {
		t.Error("matching argv must identify the engine")
	}
	reuseGuardArgv = func(int) ([]string, bool) { return []string{"some", "other"}, true }
	if engineIdentityMatches(run) {
		t.Error("an argv mismatch must veto even a matching token")
	}
	reuseGuardArgv = func(int) ([]string, bool) { return nil, false }
	if !engineIdentityMatches(run) {
		t.Error("argv-unreadable must fall back to the (matching) token")
	}
	procStartFn = func(int) (string, bool) { return "tok-0", true }
	if engineIdentityMatches(run) {
		t.Error("argv-unreadable with a mismatched token must not identify")
	}
}
