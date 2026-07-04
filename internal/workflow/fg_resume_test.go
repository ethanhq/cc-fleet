package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// writeFgTrivialScript stubs runLeaf + silences the Execute-time sweep, and returns a no-leaf script
// path — the shared setup for driving a real foreground Launch.
func writeFgTrivialScript(t *testing.T) string {
	t.Helper()
	old := runLeaf
	oldAll := sweepRunWorktreesFn
	oldOwn := sweepOwnSegmentFn
	runLeaf = func(context.Context, subagent.Request) subagent.Result {
		return subagent.Result{OK: true, Result: "ok"}
	}
	sweepRunWorktreesFn = func(string) {}
	sweepOwnSegmentFn = func(string, string) {}
	t.Cleanup(func() { runLeaf = old; sweepRunWorktreesFn = oldAll; sweepOwnSegmentFn = oldOwn })
	script := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(script, []byte(`const meta = {name: "n", description: "d", phases: [{title: "plan"}]};
phase("plan");
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestForegroundExecuteStampsFgIdentity: a real inline `workflow run --foreground` engine records its
// own pid + start token in the FOREGROUND identity fields while leaving EnginePID 0 (never
// stop-reapable) — the liveness evidence the resume guards and sweep read.
func TestForegroundExecuteStampsFgIdentity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	script := writeFgTrivialScript(t)

	id, err := Launch(context.Background(), script, Options{}, true) // fresh foreground run to completion
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	run, err := subagent.ReadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if run.EnginePID != 0 {
		t.Errorf("a foreground run must keep EnginePID 0 (not stop-reapable), got %d", run.EnginePID)
	}
	if run.FgEnginePID != os.Getpid() {
		t.Errorf("a foreground run must stamp FgEnginePID = os.Getpid() (%d), got %d", os.Getpid(), run.FgEnginePID)
	}
	if want, _ := procintrospect.ProcStart(os.Getpid()); run.FgEngineProcStart != want {
		t.Errorf("FgEngineProcStart = %q, want the process's own token %q", run.FgEngineProcStart, want)
	}
}

// fgIDCase is one recorded foreground identity + its expected resume/sweep verdicts.
type fgIDCase struct {
	name         string
	status       string
	fgPID        int
	fgTok        string
	resumeOK     bool   // resume/restart allowed (fg provably dead) vs refused
	errSub       string // expected error substring when refused
	provablyDead bool   // RunEngineProvablyNotLive → sweep reclaims vs spares
}

// fgIDCases enumerates the foreground-liveness verdicts, seeded with the TEST process's own live
// identity for the "alive" shapes (no procStartFn mock needed — ClassifyFgEngine reads the real token).
func fgIDCases(t *testing.T) []fgIDCase {
	t.Helper()
	self := os.Getpid()
	liveTok, ok := procintrospect.ProcStart(self)
	if !ok {
		t.Skip("procintrospect.ProcStart unavailable on this platform")
	}
	const deadPID = 0x7ffffffe
	return []fgIDCase{
		{"ctrl-c'd fg (stopped, fg pid dead)", "stopped", deadPID, "", true, "", true},
		{"crashed fg (running, fg pid dead)", "running", deadPID, "", true, "", true},
		{"recycled fg pid (alive, token mismatch)", "stopped", self, "stale-token", true, "", true},
		{"blind-stopped LIVE fg (alive, token match)", "stopped", self, liveTok, false, "still running in the foreground", false},
		{"unverifiable fg (alive, no token)", "stopped", self, "", false, "cannot be verified", false},
		{"absent fg identity (old record)", "stopped", 0, "", false, "cannot be verified", false},
	}
}

// TestResumeGuardByFgIdentity: a FOREGROUND resume is allowed when the recorded fg engine is provably
// dead (Ctrl-C'd / crashed / recycled) and refused otherwise (live → sharp error; unverifiable/absent
// → the conservative refusal).
func TestResumeGuardByFgIdentity(t *testing.T) {
	for _, c := range fgIDCases(t) {
		t.Run(c.name, func(t *testing.T) {
			repo := initSweepRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			script := writeFgTrivialScript(t)
			const id = "fg-resume"
			if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: c.status, EnginePID: 0, FgEnginePID: c.fgPID, FgEngineProcStart: c.fgTok}); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			_, err := Launch(context.Background(), script, Options{Resume: id}, true)
			if c.resumeOK {
				if err != nil {
					t.Errorf("resume must be allowed for a provably-dead fg engine, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("resume must be refused (%q), got %v", c.errSub, err)
			}
		})
	}
}

// TestSweepReclaimsByFgIdentity: the sweep (via RunEngineProvablyNotLive) reclaims a provably-dead
// FOREGROUND run's leaked worktree and spares a live/unverifiable one — no direct sweep change, the
// fg identity flows through RunEngineProvablyNotLive.
func TestSweepReclaimsByFgIdentity(t *testing.T) {
	for _, c := range fgIDCases(t) {
		t.Run(c.name, func(t *testing.T) {
			repo := initSweepRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees")
			const id = "fg-sweep" // path-safe
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
			if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: c.status, EnginePID: 0, FgEnginePID: c.fgPID, FgEngineProcStart: c.fgTok}); err != nil {
				t.Fatal(err)
			}
			wt := filepath.Join(tempBase, id, "wt")
			addWorktree(t, repo, wt)

			sweepRunWorktrees(repo)

			if c.provablyDead && worktreeListed(t, repo, wt) {
				t.Error("a provably-dead fg run's leaked worktree must be reclaimed")
			}
			if !c.provablyDead && !worktreeListed(t, repo, wt) {
				t.Error("a live/unverifiable fg run's worktree must be spared")
			}
		})
	}
}

// TestResumeLauncherClearsFgIdentity: resuming a run clears the PRIOR fg identity before the resumed
// engine stamps its own, so a stale identity never survives into the new engine — a foreground resume
// of a run seeded with a dead fg pid ends up carrying the RESUMED engine's own pid, not the seed.
func TestResumeLauncherClearsFgIdentity(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	script := writeFgTrivialScript(t)
	const id = "fg-clear"
	const stalePID = 0x7ffffffd // a DEAD pid distinct from the test process
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: stalePID, FgEngineProcStart: "stale"}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	run, err := subagent.ReadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if run.FgEnginePID == stalePID {
		t.Error("the resumed run must not carry the PRIOR fg identity — the launcher clears it before re-stamp")
	}
	if run.FgEnginePID != os.Getpid() {
		t.Errorf("the resumed foreground engine must stamp its OWN fg pid (%d), got %d", os.Getpid(), run.FgEnginePID)
	}
}

// TestForegroundPreflightStampsSelfIdentity: the foreground resume preflight stamps the launcher's
// OWN fg identity in the same save that flips the run running, so the crash window (a crash after the
// preflight write, before the engine's first save) never leaves a proof-less {running, 0, no-fg}
// record that resume would refuse forever. Execute is skipped (executeFn seam) to observe exactly the
// preflight write: it classifies FgAlive while this process lives (never FgUnknown).
func TestForegroundPreflightStampsSelfIdentity(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	oldExec := executeFn
	oldAll := sweepRunWorktreesFn
	oldOwn := sweepOwnSegmentFn
	executeFn = func(context.Context, string, string, Options) error { return nil } // skip the engine
	sweepRunWorktreesFn = func(string) {}
	sweepOwnSegmentFn = func(string, string) {}
	t.Cleanup(func() { executeFn = oldExec; sweepRunWorktreesFn = oldAll; sweepOwnSegmentFn = oldOwn })

	script := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(script, []byte(`const meta = {name: "n", description: "d"};`), 0o600); err != nil {
		t.Fatal(err)
	}
	const id = "fg-preflight"
	// A resumable (Ctrl-C'd fg, provably-dead) prior so the preflight proceeds to the rewrite.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0, FgEnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err != nil {
		t.Fatalf("resume: %v", err)
	}
	run, err := subagent.ReadRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if run.FgEnginePID != os.Getpid() {
		t.Errorf("the preflight must stamp the launcher's own fg pid (%d), got %d", os.Getpid(), run.FgEnginePID)
	}
	if got := subagent.ClassifyFgEngine(run); got != subagent.FgAlive {
		t.Errorf("the crash-window record must classify FgAlive (never FgUnknown), got %v", got)
	}
}
