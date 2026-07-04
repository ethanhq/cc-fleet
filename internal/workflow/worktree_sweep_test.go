package workflow

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// initSweepRepo makes a committed git repo and chdirs into it, returning its root.
func initSweepRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := runGit(repo, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(repo, "add", "."); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runGit(repo, "commit", "-qm", "init"); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	return repo
}

// addWorktree registers a detached worktree at path from repo.
func addWorktree(t *testing.T, repo, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(repo, "worktree", "add", "--detach", path, "HEAD"); err != nil {
		t.Fatalf("worktree add %s: %v %s", path, err, out)
	}
}

// worktreeListed reports whether path appears in `git worktree list`. BOTH the porcelain output and
// the sought path are canonicalized (canonPath: macOS /private, Windows 8.3→long) and normalized
// (normPath: forward slashes + case-fold on Windows). Folding only the sought side — as a naive
// Contains(out, normPath(path)) does — misses on Windows, where porcelain reports mixed-case paths
// (the production pathUnder already folds both sides, so this only fixes the test lookup).
func worktreeListed(t *testing.T, repo, path string) bool {
	t.Helper()
	out, err := runGit(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v %s", err, out)
	}
	return strings.Contains(normPath(out), normPath(canonPath(path)))
}

// TestSweepRunWorktreesScoped: the Execute-time sweep applies one uniform rule — remove a
// registration only under a DEATH PROOF: a KNOWN provably-dead run (clause a) or a vanished workdir
// (clause b). A live/known-not-dead run, an UNKNOWN still-present segment (another config store may
// own it), and the user's own worktree are all left untouched. It never privileges its own segment.
func TestSweepRunWorktreesScoped(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees") // match git porcelain (macOS /private, win long path)

	const (
		runDead        = "run-dead"
		runLive        = "run-live"
		runFgStopped   = "run-fg-stopped"   // foreground blind-flipped to stopped (pid 0) → spared
		runUnknownHere = "run-unknown"      // no manifest, workdir intact → spared
		runUnknownGone = "run-unknown-gone" // no manifest, workdir gone → swept (clause b)
	)
	t.Cleanup(func() {
		for _, id := range []string{runDead, runLive, runFgStopped, runUnknownHere, runUnknownGone} {
			_ = os.RemoveAll(filepath.Join(tempBase, id))
		}
	})

	// runDead models a DETACHED run StopRun reaped: "stopped" with its now-dead pid retained (the
	// death evidence) → provably dead. runLive is a running foreground engine (EnginePID 0), and
	// runFgStopped is a foreground run blind-flipped to "stopped" (EnginePID 0, nothing reaped) —
	// neither is provably dead, so both must be spared.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: runDead, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: runLive, StartedAt: "2026-01-01T00:00:00Z", Status: "running", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: runFgStopped, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}

	// A provably-dead KNOWN run whose workdir is PRESENT — swept via clause a (a death proof, not
	// an own-segment privilege).
	deadWt := filepath.Join(tempBase, runDead, "wt")
	addWorktree(t, repo, deadWt)
	// A live KNOWN run's registration — must survive.
	liveWt := filepath.Join(tempBase, runLive, "wt")
	addWorktree(t, repo, liveWt)
	// A FOREGROUND run externally flipped to "stopped" (EnginePID 0 — nothing reaped): NOT provably
	// dead, so its still-present registration must survive (its engine may still be live).
	fgStoppedWt := filepath.Join(tempBase, runFgStopped, "wt")
	addWorktree(t, repo, fgStoppedWt)
	// The user's OWN worktree, elsewhere (never under the temp prefix) — must survive.
	userWt := filepath.Join(t.TempDir(), "user-wt")
	addWorktree(t, repo, userWt)
	// An UNKNOWN segment (no manifest) whose workdir EXISTS — another config store may own a live
	// run we can't see, so it must be left alone.
	unknownHereWt := filepath.Join(tempBase, runUnknownHere, "wt")
	addWorktree(t, repo, unknownHereWt)
	// An UNKNOWN segment whose workdir is GONE — prunable (clause b), swept with no record to consult.
	unknownGoneWt := filepath.Join(tempBase, runUnknownGone, "wt")
	addWorktree(t, repo, unknownGoneWt)
	if err := os.RemoveAll(unknownGoneWt); err != nil {
		t.Fatal(err)
	}

	sweepRunWorktrees(repo)

	if worktreeListed(t, repo, deadWt) {
		t.Error("a provably-dead known run's registration must be swept (clause a)")
	}
	if !worktreeListed(t, repo, liveWt) {
		t.Error("a LIVE known run's registration must be left untouched")
	}
	if !worktreeListed(t, repo, fgStoppedWt) {
		t.Error("a foreground run blind-flipped to stopped (EnginePID 0) must be left alone — its engine may still be live")
	}
	if !worktreeListed(t, repo, userWt) {
		t.Error("the user's own worktree must never be touched")
	}
	if !worktreeListed(t, repo, unknownHereWt) {
		t.Error("an unknown segment whose workdir still exists must be left alone (another store may own it)")
	}
	if worktreeListed(t, repo, unknownGoneWt) {
		t.Error("an unknown segment whose workdir is gone must be swept (clause b)")
	}
}

// TestSweepRunWorktreesNonGit: a non-git root is a best-effort no-op (no panic).
func TestSweepRunWorktreesNonGit(t *testing.T) {
	sweepRunWorktrees(t.TempDir())
}

// TestSweepRunWorktreesMapGuard: ids.WorktreeSegment collapses '.'/':'/separators to '-', so ids
// "a.b"/"a:b"/"a-b" share segment "a-b". The segment verdict reclaims only under a PATH-SAFE
// dead+leaf-free owner, so a DEAD non-path-safe twin can't provoke reclaiming a live run that maps to it.
func TestSweepRunWorktreesMapGuard(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees") // match git porcelain (macOS /private, win long path)
	const liveID = "a-b"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, liveID)) })

	// A DEAD twin "a.b" (older StartedAt → the LAST, overwriting insert into a segment-keyed map
	// without the guard) and a LIVE "a-b" (newer). The path-safe map guard drops the dead twin, so
	// "a-b"'s own liveness stands and its registration survives.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: "a.b", StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: liveID, StartedAt: "2026-01-02T00:00:00Z", Status: "running", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}
	liveWt := filepath.Join(tempBase, liveID, "wt")
	addWorktree(t, repo, liveWt)

	sweepRunWorktrees(repo)

	if !worktreeListed(t, repo, liveWt) {
		t.Error("a dead colliding twin must not condemn the live run's registration (map guard)")
	}
	if _, err := os.Stat(liveWt); err != nil {
		t.Errorf("the live run's workdir must survive (err=%v)", err)
	}
}

// TestSweepOwnSegment: the launcher's own-segment reclaimer removes a PATH-SAFE run's registrations
// and temp dir, and is a no-op for a non-path-safe id (whose sanitized segment collides with others).
func TestSweepOwnSegment(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees") // match git porcelain (macOS /private, win long path)

	t.Run("path-safe id reclaims its own segment", func(t *testing.T) {
		const id = "own-seg"
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
		// The launcher calls sweepOwnSegment only after confirming the prior run provably dead; the
		// verdict now requires that KNOWN dead+leaf-free owner (fail-closed on a missing entry).
		if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(tempBase, id, "wt")
		addWorktree(t, repo, wt)

		sweepOwnSegment(repo, id)

		if worktreeListed(t, repo, wt) {
			t.Error("own-segment registration must be reclaimed")
		}
		if _, err := os.Stat(filepath.Join(tempBase, id)); !os.IsNotExist(err) {
			t.Errorf("own-segment temp dir must be removed (err=%v)", err)
		}
	})

	t.Run("non-path-safe id is a no-op (colliding segment spared)", func(t *testing.T) {
		const collidingID = "a-b" // the segment "a.b" would sanitize into
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, collidingID)) })
		wt := filepath.Join(tempBase, collidingID, "wt")
		addWorktree(t, repo, wt)

		sweepOwnSegment(repo, "a.b") // non-path-safe → must not touch the colliding "a-b" segment

		if !worktreeListed(t, repo, wt) {
			t.Error("a non-path-safe id must not reclaim a colliding segment")
		}
		if _, err := os.Stat(wt); err != nil {
			t.Errorf("the colliding segment's workdir must survive (err=%v)", err)
		}
	})
}

// TestPurgeThenSweepReclaimsRegistration: purging a provably-dead run drops its worktree WORKDIR
// before the manifest (PurgeRun does no git ops by design), so the leftover git registration is
// reclaimed by a later same-repo sweep via the workdir-missing clause.
func TestPurgeThenSweepReclaimsRegistration(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees") // match git porcelain (macOS /private, win long path)

	const id = "purge-reclaim" // path-safe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	// A provably-dead detached run (stopped + retained dead pid) with a real registered worktree.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt)

	if err := subagent.PurgeRun(id); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}
	// Workdir + manifest gone; the git registration lingers (PurgeRun touches no git state).
	if _, err := os.Stat(filepath.Join(tempBase, id)); !os.IsNotExist(err) {
		t.Errorf("worktree workdir must be gone after purge (err=%v)", err)
	}
	if _, err := subagent.ReadRun(id); err == nil {
		t.Error("run manifest must be gone after purge")
	}
	if !worktreeListed(t, repo, wt) {
		t.Fatal("precondition: the git registration should still be listed (purge does no git ops)")
	}

	sweepRunWorktrees(repo)

	if worktreeListed(t, repo, wt) {
		t.Error("the leftover registration must be reclaimed by the workdir-missing clause after purge")
	}
}

// writeTrivialScript writes a no-leaf workflow script and returns its path + a minted run.
func writeTrivialScript(t *testing.T) (string, subagent.WorkflowRun) {
	t.Helper()
	old := runLeaf
	runLeaf = func(context.Context, subagent.Request) subagent.Result {
		return subagent.Result{OK: true, Result: "ok"}
	}
	t.Cleanup(func() { runLeaf = old })
	script := filepath.Join(t.TempDir(), "wf.js")
	if err := os.WriteFile(script, []byte(`const meta = {name: "n", description: "d", phases: [{title: "plan"}]};
phase("plan");
`), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return script, run
}

// TestSweepEngineWiring (seam): Execute fires the Execute-time sweep exactly once with root = the
// repo top-level (it no longer passes a run id — the sweep has no own-segment clause); a non-git cwd
// skips it and the run still succeeds.
func TestSweepEngineWiring(t *testing.T) {
	t.Run("git cwd fires sweep", func(t *testing.T) {
		repo := initSweepRepo(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())

		var gotRoot string
		var calls int
		old := sweepRunWorktreesFn
		sweepRunWorktreesFn = func(root string) { calls++; gotRoot = root }
		t.Cleanup(func() { sweepRunWorktreesFn = old })

		script, run := writeTrivialScript(t)
		t.Chdir(repo)
		if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if calls != 1 {
			t.Fatalf("sweep called %d times, want 1", calls)
		}
		if normPath(gotRoot) != normPath(canonPath(repo)) { // gitTopLevel canonicalizes; match it
			t.Errorf("sweep root = %q, want repo %q", gotRoot, repo)
		}
	})

	t.Run("non-git cwd skips sweep", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		var calls int
		old := sweepRunWorktreesFn
		sweepRunWorktreesFn = func(string) { calls++ }
		t.Cleanup(func() { sweepRunWorktreesFn = old })

		script, run := writeTrivialScript(t)
		t.Chdir(t.TempDir()) // not a git repo
		if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if calls != 0 {
			t.Errorf("sweep must be skipped on a non-git cwd, got %d calls", calls)
		}
	})
}

// TestResumeLauncherOwnSegmentSweep: resuming a PROVABLY-DEAD prior reclaims the run's OWN
// isolation-worktree segment in the launcher — the last point the death evidence exists, before the
// manifest rewrite. Both a properly-stopped detached prior ({stopped, dead pid retained}) and a
// crashed detached prior ({running, dead pid}) are reclaimed. (A {stopped, pid 0} prior is refused
// outright — see TestResumeStoppedZeroPidRefused.)
func TestResumeLauncherOwnSegmentSweep(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		enginePID int
	}{
		{"stopped detached (dead pid retained)", "stopped", 0x7ffffffe},
		{"crashed detached (running + dead pid)", "running", 0x7ffffffe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := initSweepRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())

			// Seam both sweeps: assert the own-segment one, silence the Execute-time one.
			var gotRoot, gotRun string
			var ownCalls int
			oldOwn := sweepOwnSegmentFn
			oldAll := sweepRunWorktreesFn
			sweepOwnSegmentFn = func(root, runID string) { ownCalls++; gotRoot, gotRun = root, runID }
			sweepRunWorktreesFn = func(string) {}
			t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })

			script, _ := writeTrivialScript(t) // stubs runLeaf; we resume a manifest we seed below
			const id = "resume-own"
			if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: c.status, EnginePID: c.enginePID}); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err != nil {
				t.Fatalf("launch resume: %v", err)
			}
			if ownCalls != 1 {
				t.Fatalf("own-segment sweep fired %d times, want 1 (prior provably dead)", ownCalls)
			}
			if gotRun != id {
				t.Errorf("own sweep runID = %q, want %q", gotRun, id)
			}
			if normPath(gotRoot) != normPath(canonPath(repo)) { // gitTopLevel canonicalizes; match it
				t.Errorf("own sweep root = %q, want repo %q", gotRoot, repo)
			}
		})
	}
}

// TestResumeStoppedZeroPidRefused: a {stopped, EnginePID 0} record is unresumable on every entry —
// foreground resume, detached resume, and board Restart — because a live blind-stopped foreground
// engine is indistinguishable from a dead Ctrl-C'd one. The refusal fires in the shared preflight
// BEFORE any own-segment sweep, so nothing is reclaimed.
func TestResumeStoppedZeroPidRefused(t *testing.T) {
	const wantErr = "cannot be verified"
	// seed a {stopped,0} run, chdir into a fresh repo, and seam the sweeps to counters.
	seed := func(t *testing.T) (id string, sweeps *int32) {
		repo := initSweepRepo(t)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		id = "resume-stopped0"
		if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0}); err != nil {
			t.Fatal(err)
		}
		var n int32
		oldOwn := sweepOwnSegmentFn
		oldAll := sweepRunWorktreesFn
		sweepOwnSegmentFn = func(string, string) { atomic.AddInt32(&n, 1) }
		sweepRunWorktreesFn = func(string) {}
		t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })
		t.Chdir(repo)
		return id, &n
	}

	t.Run("foreground resume", func(t *testing.T) {
		id, sweeps := seed(t)
		script, _ := writeTrivialScript(t)
		if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("foreground resume of {stopped,0} must be refused, got %v", err)
		}
		if *sweeps != 0 {
			t.Errorf("a refused resume must not sweep, got %d", *sweeps)
		}
	})

	t.Run("detached resume", func(t *testing.T) {
		id, sweeps := seed(t)
		script, _ := writeTrivialScript(t)
		// Detached Launch reaches the same preflight; the refusal returns before launchDetached.
		if _, err := Launch(context.Background(), script, Options{Resume: id}, false); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("detached resume of {stopped,0} must be refused, got %v", err)
		}
		if *sweeps != 0 {
			t.Errorf("a refused resume must not sweep, got %d", *sweeps)
		}
	})

	t.Run("board restart", func(t *testing.T) {
		id, sweeps := seed(t)
		script, _ := writeTrivialScript(t)
		// Restart reads the saved-script sidecar and flows through Launch's resume branch.
		sp, err := subagent.RunScriptPath(id)
		if err != nil {
			t.Fatal(err)
		}
		data, rerr := os.ReadFile(script)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(sp, data, 0o600); werr != nil {
			t.Fatal(werr)
		}
		if err := Restart(context.Background(), id, ""); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("board Restart of {stopped,0} must be refused, got %v", err)
		}
		if *sweeps != 0 {
			t.Errorf("a refused restart must not sweep, got %d", *sweeps)
		}
	})
}

// TestRestartStoppedZeroPidRefusedNoJournalMutation: a {stopped, EnginePID 0} record is refused by
// BOTH leaf restart and phase restart, and the refusal fires in ensureRestartable BEFORE any journal
// rewrite — the journal file is byte-identical after (no mutate-then-fail half-state). A real one-leaf
// "plan"-phase run seeds a removable journal key + a member job carrying it + the script sidecar, so
// a restart WOULD rewrite the journal if it weren't refused first (else the assertion couldn't tell
// "guarded" from "nothing to drop").
func TestRestartStoppedZeroPidRefusedNoJournalMutation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	const wantErr = "cannot be verified"

	old := runLeaf
	oldAll := sweepRunWorktreesFn
	runLeaf = func(context.Context, subagent.Request) subagent.Result {
		return subagent.Result{OK: true, Result: "ok"}
	}
	sweepRunWorktreesFn = func(string) {} // silence the Execute-time sweep during the seed run
	t.Cleanup(func() { runLeaf = old; sweepRunWorktreesFn = oldAll })

	script := filepath.Join(t.TempDir(), "s.js")
	if err := os.WriteFile(script, []byte(`const meta = {name: "n", description: "d", phases: [{title: "plan"}]};
phase("plan");
await agent("x", {provider: "v"});
`), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := Launch(context.Background(), script, Options{}, true) // fresh foreground run to completion
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	jp, err := subagent.RunJournalPath(id)
	if err != nil {
		t.Fatal(err)
	}
	before, rerr := os.ReadFile(jp)
	if rerr != nil || len(before) == 0 {
		t.Fatalf("precondition: the run must have journaled its leaf (err=%v, bytes=%d)", rerr, len(before))
	}
	_, leaves, serr := subagent.RunStatus(id)
	if serr != nil {
		t.Fatal(serr)
	}
	var leafKey string
	for _, l := range leaves {
		if l.JournalKey != "" {
			leafKey = l.JournalKey
			break
		}
	}
	if leafKey == "" {
		t.Fatal("precondition: expected a member leaf with a journal key")
	}
	// Force the completed run into the ambiguous OLD-record {stopped, 0, no fg identity} state — the
	// pre-field transition shape (clear the fg fields the real run just stamped) → FgUnknown → refused.
	run, _ := subagent.ReadRun(id)
	run.Status, run.EnginePID = "stopped", 0
	run.FgEnginePID, run.FgEngineProcStart = 0, ""
	if err := subagent.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	assertUnmutated := func(t *testing.T) {
		after, err := os.ReadFile(jp)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("journal must be byte-identical after a refused restart\nbefore=%q\nafter =%q", before, after)
		}
	}

	t.Run("leaf restart", func(t *testing.T) {
		if err := Restart(context.Background(), id, leafKey); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("leaf restart of {stopped,0} must be refused, got %v", err)
		}
		assertUnmutated(t)
	})

	t.Run("phase restart", func(t *testing.T) {
		if _, err := RestartPhase(context.Background(), id, "plan"); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("phase restart of {stopped,0} must be refused, got %v", err)
		}
		assertUnmutated(t)
	})
}

// TestBlindStoppedForegroundChainWorktreeSurvives: the end-to-end chain. A blind-stopped live
// foreground run ({stopped,0}) with a real worktree in its segment: resume is REFUSED (no twin engine
// is ever created), and a subsequent Execute-time sweep spares the segment ({stopped,0} is not
// provably dead) — so the (possibly still-live) worktree survives. The chain's destructive half never
// fires.
func TestBlindStoppedForegroundChainWorktreeSurvives(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	tempBase := filepath.Join(canonPath(os.TempDir()), "cc-fleet-worktrees") // match git porcelain (macOS /private, win long path)

	const id = "chain-fg"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0}); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(tempBase, id, "wt")
	addWorktree(t, repo, wt)

	script, _ := writeTrivialScript(t)
	t.Chdir(repo)
	if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err == nil || !strings.Contains(err.Error(), "cannot be verified") {
		t.Fatalf("resume of the blind-stopped foreground run must be refused, got %v", err)
	}

	sweepRunWorktrees(repo)

	if !worktreeListed(t, repo, wt) {
		t.Error("the blind-stopped foreground run's worktree must survive the sweep (not provably dead)")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("the worktree workdir must survive (err=%v)", err)
	}
}

// TestResumeLegitStatesAllowed: the {stopped,0} refusal must NOT catch legitimately-resumable states
// — a completed foreground run ({done,0}), a crashed detached run ({running, dead pid}), and a
// properly-stopped detached run ({stopped, dead pid>0}) all resume without the refusal.
func TestResumeLegitStatesAllowed(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		enginePID int
	}{
		{"completed foreground {done,0}", "done", 0},
		{"crashed detached {running, dead pid}", "running", 0x7ffffffe},
		{"stopped detached {stopped, dead pid}", "stopped", 0x7ffffffe},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo := initSweepRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("HOME", t.TempDir())
			oldOwn := sweepOwnSegmentFn
			oldAll := sweepRunWorktreesFn
			sweepOwnSegmentFn = func(string, string) {}
			sweepRunWorktreesFn = func(string) {}
			t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })

			script, _ := writeTrivialScript(t)
			const id = "resume-legit"
			if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: c.status, EnginePID: c.enginePID}); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
			if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err != nil {
				t.Errorf("resume of %s must be allowed, got %v", c.name, err)
			}
		})
	}
}

// TestResumeConcurrentForegroundResumesSweepOnce: two foreground resumes of the same provably-dead
// run race. The foreground preflight self-serializes under the per-run lock, so exactly ONE resumer
// sweeps the dead prior's own segment (the other, seeing the manifest already flipped to "running",
// is refused) and the two preflights never sweep concurrently — closing the stale-death-proof TOCTOU
// where the loser would delete the winner's live worktrees.
func TestResumeConcurrentForegroundResumesSweepOnce(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const id = "resume-race"
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	script, _ := writeTrivialScript(t)

	// The own-segment sweep IS the protected operation — use its seam to count invocations and detect
	// any concurrent overlap, widening the window so an unserialized race would reliably double-sweep.
	var sweeps, inSweep, overlap int32
	oldOwn := sweepOwnSegmentFn
	oldAll := sweepRunWorktreesFn
	sweepOwnSegmentFn = func(string, string) {
		if atomic.AddInt32(&inSweep, 1) > 1 {
			atomic.StoreInt32(&overlap, 1)
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&sweeps, 1)
		atomic.AddInt32(&inSweep, -1)
	}
	sweepRunWorktreesFn = func(string) {} // silence the Execute-time sweep during the concurrent runs
	t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })

	t.Chdir(repo)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = Launch(context.Background(), script, Options{Resume: id}, true) }()
	}
	wg.Wait()

	if overlap != 0 {
		t.Error("two foreground resume preflights swept concurrently — the per-run lock did not serialize them")
	}
	if sweeps != 1 {
		t.Errorf("own-segment sweep fired %d times, want exactly 1 (only one resumer reclaims the dead prior)", sweeps)
	}
}

// TestResumeForegroundSerializesAgainstHeldLock: a foreground resume must block on the per-run lock
// while a detached-lane holder (as the CLI/Restart wrap Launch) owns it, then — once the holder has
// flipped the manifest to "running" and released — be refused by the resume guard without sweeping.
// This is the cross-lane serialization: the foreground self-lock and the caller-held lock contend on
// the same runs/<id>.lock.
func TestResumeForegroundSerializesAgainstHeldLock(t *testing.T) {
	repo := initSweepRepo(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	const id = "resume-held"
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}
	script, _ := writeTrivialScript(t)

	var sweeps int32
	oldOwn := sweepOwnSegmentFn
	oldAll := sweepRunWorktreesFn
	sweepOwnSegmentFn = func(string, string) { atomic.AddInt32(&sweeps, 1) }
	sweepRunWorktreesFn = func(string) {}
	t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })

	t.Chdir(repo)
	// A holder of the per-run lock that wins the preflight: it flips the run to a running state with no
	// verifiable engine identity ({running, 0, no fg} → FgUnknown) before releasing, mirroring a winner
	// whose fg identity is not yet re-stamped. The loser must block on the lock, then be refused.
	holding, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = subagent.WithRunLock(id, func() error {
			close(holding)
			<-release
			r, _ := subagent.ReadRun(id)
			r.Status, r.EnginePID = "running", 0
			r.FgEnginePID, r.FgEngineProcStart = 0, ""
			return subagent.SaveRun(r)
		})
	}()
	<-holding

	done := make(chan error, 1)
	go func() { _, e := Launch(context.Background(), script, Options{Resume: id}, true); done <- e }()
	select {
	case <-done:
		t.Fatal("foreground resume completed while the per-run lock was held — not serialized")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "foreground") {
		t.Errorf("foreground resume must be refused after the holder flipped the run to running, got %v", err)
	}
	if sweeps != 0 {
		t.Errorf("the refused foreground resume must not sweep, got %d", sweeps)
	}
}
