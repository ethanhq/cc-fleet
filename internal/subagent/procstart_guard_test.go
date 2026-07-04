package subagent

import (
	"os"
	"testing"
)

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
