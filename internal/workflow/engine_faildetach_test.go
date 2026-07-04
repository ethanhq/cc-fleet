package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestExecuteFatalFirstStamp (codex r21): Execute's FIRST identity stamp is fatal — if it can't persist
// (disk full / EPERM), Execute returns the error BEFORE the startup sweep and any leaf/worktree work.
// This upholds the invariant finalizeFailedDetach relies on: a manifest still reading EnginePID 0 means
// the child created NO worktrees (so restoring the prior death proof strands nothing).
func TestExecuteFatalFirstStamp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	origSave := saveRunFn
	saveRunFn = func(subagent.WorkflowRun) error { return fmt.Errorf("disk full") } // fail the manifest write
	origSweep := sweepRunWorktreesFn
	origLeaf := runLeaf
	swept, ranLeaf := false, false
	sweepRunWorktreesFn = func(string) { swept = true }
	runLeaf = func(context.Context, subagent.Request) subagent.Result {
		ranLeaf = true
		return subagent.Result{OK: true}
	}
	t.Cleanup(func() { saveRunFn = origSave; sweepRunWorktreesFn = origSweep; runLeaf = origLeaf })

	script := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(script, []byte(`const meta = {name:"n",description:"d",phases:[{title:"p"}]};
phase("p");
`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Execute(context.Background(), script, "fatal-run", Options{RunID: "fatal-run"})
	if err == nil {
		t.Error("Execute must FAIL when the first identity stamp cannot persist")
	}
	if swept {
		t.Error("Execute must NOT run the startup sweep when the first stamp failed")
	}
	if ranLeaf {
		t.Error("Execute must NOT run any leaf (create worktrees) when the first stamp failed")
	}
}

// TestFinalizeFailedDetach (codex r20): a failed detached launch must not destroy a death proof.
// (a) child self-stamped (EnginePID != 0) → mark failed, KEEP the pid (death evidence, already reaped);
// (b) RESUME whose child never stamped (EnginePID 0) → restore the PRIOR record's death proof;
// (c) FRESH launch (no prior) → mark the minted run failed.
func TestFinalizeFailedDetach(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	const ts = "2026-01-01T00:00:00Z"

	// (a) child stamped → failed + pid kept.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "stamped", StartedAt: ts, Status: "running", EnginePID: 4242}); err != nil {
		t.Fatal(err)
	}
	finalizeFailedDetach("stamped", subagent.WorkflowRun{RunID: "stamped"}, nil, "boom")
	if got, _ := subagent.ReadRun("stamped"); got.Status != "failed" || got.EnginePID != 4242 {
		t.Errorf("child-stamped: want {failed, pid 4242}, got {%s, %d}", got.Status, got.EnginePID)
	}

	// (b) resume, child never stamped → prior death proof restored.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "resume", StartedAt: ts, Status: "running", EnginePID: 0}); err != nil { // the preflight's write
		t.Fatal(err)
	}
	prior := subagent.WorkflowRun{RunID: "resume", StartedAt: ts, Status: "stopped", EnginePID: 0x7ffffffe, EngineProcStart: "tok"}
	finalizeFailedDetach("resume", subagent.WorkflowRun{RunID: "resume"}, &prior, "boom")
	if got, _ := subagent.ReadRun("resume"); got.EnginePID != 0x7ffffffe || got.EngineProcStart != "tok" {
		t.Errorf("resume-no-stamp: want the prior death proof restored, got {pid %d, tok %q}", got.EnginePID, got.EngineProcStart)
	}

	// (c) fresh, no prior → minted marked failed.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "fresh", StartedAt: ts, Status: "running", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}
	finalizeFailedDetach("fresh", subagent.WorkflowRun{RunID: "fresh", StartedAt: ts, Status: "running"}, nil, "boom")
	if got, _ := subagent.ReadRun("fresh"); got.Status != "failed" {
		t.Errorf("fresh: want failed, got %s", got.Status)
	}
}

// TestFailedDetachedResumeRestoresPriorDeathProof (codex r20): end-to-end via the launchDetachedFn
// seam — a detached RESUME whose spawn fails must restore the PRIOR record so its death proof survives
// (the manifest reads provably dead again → the sweep can still reclaim its worktrees), not a
// {failed,0,no-fg} that would strand them forever.
func TestFailedDetachedResumeRestoresPriorDeathProof(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	script := writeFgTrivialScript(t) // stubs runLeaf + both sweep fns
	const id = "resume-fail"
	const deadPID = 0x7ffffffe
	prior := subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadPID, EngineProcStart: "tok"}
	if err := subagent.SaveRun(prior); err != nil {
		t.Fatal(err)
	}

	origLaunch := launchDetachedFn
	launchDetachedFn = func(string, string, Options) (int, *detachedReaper, error) {
		return 0, nil, fmt.Errorf("boom: spawn failed")
	}
	t.Cleanup(func() { launchDetachedFn = origLaunch })

	if _, err := Launch(context.Background(), script, Options{Resume: id}, false); err == nil {
		t.Fatal("Launch must fail when the detached spawn fails")
	}
	got, rerr := subagent.ReadRun(id)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got.EnginePID != deadPID || got.EngineProcStart != "tok" {
		t.Errorf("failed detached resume must RESTORE the prior death proof, got EnginePID=%d token=%q status=%q", got.EnginePID, got.EngineProcStart, got.Status)
	}
	if !subagent.RunEngineProvablyNotLive(got) {
		t.Error("the restored record must read provably dead so the sweep can still reclaim its worktrees")
	}
}
