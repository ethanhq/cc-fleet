package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestExecuteFatalFirstStamp: Execute's FIRST identity stamp is fatal — if it can't persist
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
	sweepRunWorktreesFn = func(string) []string { swept = true; return nil }
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

// TestFinalizeFailedDetach: a failed detached launch must not destroy a death proof.
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
	finalizeFailedDetach("stamped", subagent.WorkflowRun{RunID: "stamped"}, nil, "boom", "")
	if got, _ := subagent.ReadRun("stamped"); got.Status != "failed" || got.EnginePID != 4242 {
		t.Errorf("child-stamped: want {failed, pid 4242}, got {%s, %d}", got.Status, got.EnginePID)
	}

	// (b) resume, child never stamped → prior death proof restored.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "resume", StartedAt: ts, Status: "running", EnginePID: 0}); err != nil { // the preflight's write
		t.Fatal(err)
	}
	prior := subagent.WorkflowRun{RunID: "resume", StartedAt: ts, Status: "stopped", EnginePID: 0x7ffffffe, EngineProcStart: "tok"}
	finalizeFailedDetach("resume", subagent.WorkflowRun{RunID: "resume"}, &prior, "boom", "")
	if got, _ := subagent.ReadRun("resume"); got.EnginePID != 0x7ffffffe || got.EngineProcStart != "tok" {
		t.Errorf("resume-no-stamp: want the prior death proof restored, got {pid %d, tok %q}", got.EnginePID, got.EngineProcStart)
	}

	// (c) fresh, no prior → minted marked failed.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "fresh", StartedAt: ts, Status: "running", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}
	finalizeFailedDetach("fresh", subagent.WorkflowRun{RunID: "fresh", StartedAt: ts, Status: "running"}, nil, "boom", "")
	if got, _ := subagent.ReadRun("fresh"); got.Status != "failed" {
		t.Errorf("fresh: want failed, got %s", got.Status)
	}
}

// TestFailedDetachedResumeRestoresPriorDeathProof: end-to-end via the launchDetachedFn
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

// TestDetachedResumeSurfacesPreStartCause: a detached child that fails before registering (its run
// directory vanished between the resume preflight and Execute's chdir) records why; the launcher must
// return that cause at once instead of waiting out the startup budget and replying only "engine failed
// to start", and must record it on the restored prior record without losing the death proof.
func TestDetachedResumeSurfacesPreStartCause(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	script := writeFgTrivialScript(t)
	const id = "resume-cwd-gone"
	const deadPID = 0x7ffffffe
	runDir := t.TempDir()
	prior := subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "failed", Error: "old failure",
		EnginePID: deadPID, EngineProcStart: "tok", Cwd: runDir}
	if err := subagent.SaveRun(prior); err != nil {
		t.Fatal(err)
	}

	origLaunch := launchDetachedFn
	launchDetachedFn = func(script, runID string, _ Options) (int, *detachedReaper, error) {
		// Stand in for the detached child: the real Execute, which cannot enter the run directory
		// and records that in the manifest before it would stamp its pid.
		if err := os.RemoveAll(runDir); err != nil {
			return 0, nil, err
		}
		_ = Execute(context.Background(), script, runID, Options{RunID: runID})
		cmd := exec.Command(os.Args[0], "-test.run=^$") // a real process for the reaper to kill + reap
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			return 0, nil, err
		}
		if err := cmd.Start(); err != nil {
			devnull.Close()
			return 0, nil, err
		}
		return cmd.Process.Pid, &detachedReaper{cmd: cmd, devnull: devnull}, nil
	}
	t.Cleanup(func() { launchDetachedFn = origLaunch })

	start := time.Now()
	_, err := Launch(context.Background(), script, Options{Resume: id}, false)
	if err == nil || !strings.Contains(err.Error(), "engine failed to start") || !strings.Contains(err.Error(), "run directory") {
		t.Fatalf("Launch error = %v, want the child's run-directory cause", err)
	}
	if el := time.Since(start); el >= engineStartupBudget {
		t.Errorf("Launch waited %v; a recorded pre-start failure must end the wait early", el)
	}
	got, rerr := subagent.ReadRun(id)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got.Status != "failed" || !strings.Contains(got.Error, "run directory") {
		t.Errorf("manifest = {%s, %q}, want failed with the run-directory cause", got.Status, got.Error)
	}
	if got.EnginePID != deadPID || got.EngineProcStart != "tok" {
		t.Errorf("the prior death proof must survive, got EnginePID=%d token=%q", got.EnginePID, got.EngineProcStart)
	}
}

// TestResumeRefusesBrokenScript: a --resume with a script the engine could not start (a parse error,
// a bad meta, or an error only the compiler reports) fails at once and leaves the run as it was — the
// saved script restart runs, the manifest, and no engine launched — so a later restart still works.
func TestResumeRefusesBrokenScript(t *testing.T) {
	broken := map[string]string{
		"parse":   "const meta = {name: \"n\", description: \"d\"};\nconst x = ;\n",
		"meta":    "const meta = {name: \"n\"};\nphase(\"plan\");\n",
		"compile": "const meta = {name: \"n\", description: \"d\"};\nlet a = 1;\nlet a = 2;\n",
	}
	for _, fg := range []bool{false, true} {
		for kind, src := range broken {
			t.Run(map[bool]string{false: "detached", true: "foreground"}[fg]+"/"+kind, func(t *testing.T) {
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				t.Setenv("HOME", t.TempDir())
				id, err := Launch(context.Background(), writeFgTrivialScript(t), Options{}, true)
				if err != nil {
					t.Fatal(err)
				}
				// A provably dead engine, so a resume that got past the check would sweep its worktrees.
				run, err := subagent.ReadRun(id)
				if err != nil {
					t.Fatal(err)
				}
				run.EnginePID, run.EngineProcStart = 0x7ffffffe, "tok"
				if err := subagent.SaveRun(run); err != nil {
					t.Fatal(err)
				}
				sp, _ := subagent.RunScriptPath(id)
				mp := filepath.Join(filepath.Dir(sp), id+".json")
				manifest, _ := os.ReadFile(mp)
				saved, _ := os.ReadFile(sp)

				origLaunch, origExec, origSweep := launchDetachedFn, executeFn, sweepOwnSegmentFn
				launched, swept := false, false
				launchDetachedFn = func(string, string, Options) (int, *detachedReaper, error) {
					launched = true
					return 0, nil, fmt.Errorf("no engine may be launched")
				}
				executeFn = func(context.Context, string, string, Options) error { launched = true; return nil }
				sweepOwnSegmentFn = func(string, string) { swept = true }
				t.Cleanup(func() { launchDetachedFn, executeFn, sweepOwnSegmentFn = origLaunch, origExec, origSweep })

				bad := filepath.Join(t.TempDir(), "bad.js")
				if err := os.WriteFile(bad, []byte(src), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Launch(context.Background(), bad, Options{Resume: id}, fg); err == nil {
					t.Fatal("a resume with a broken script must fail")
				}
				if launched {
					t.Error("no engine may be launched for a broken script")
				}
				if swept {
					t.Error("a broken script must be refused before the run's worktrees are swept")
				}
				if got, _ := os.ReadFile(sp); !bytes.Equal(got, saved) {
					t.Error("the run's saved script must not be replaced by a broken one")
				}
				if got, _ := os.ReadFile(mp); !bytes.Equal(got, manifest) {
					t.Errorf("the manifest must be untouched:\nwas %s\nnow %s", manifest, got)
				}
			})
		}
	}
}
