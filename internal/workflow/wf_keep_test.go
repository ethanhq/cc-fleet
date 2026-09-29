package workflow

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// wfIsolate points the config store and HOME at temp dirs and pins the version gate, so an
// Execute-driven test never reads the host's claude or config.
func wfIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
}

// wfStubLeaf swaps runLeaf for fn for the test's lifetime.
func wfStubLeaf(t *testing.T, fn func(context.Context, subagent.Request) subagent.Result) {
	t.Helper()
	old := runLeaf
	runLeaf = fn
	t.Cleanup(func() { runLeaf = old })
}

// wfCaptureStderr runs fn with os.Stderr redirected to a pipe and returns what was written.
func wfCaptureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	fn()
	os.Stderr = old
	_ = w.Close()
	return string(<-done)
}

// wfCorruptIndex overwrites wt's own index so every later `git status` in it fails. Non-fatal
// (t.Errorf): it also runs on a leaf goroutine.
func wfCorruptIndex(t *testing.T, wt string) {
	t.Helper()
	gd, err := runGit(wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Errorf("rev-parse --absolute-git-dir: %v %s", err, gd)
		return
	}
	if err := os.WriteFile(filepath.Join(strings.TrimSpace(gd), "index"), []byte("garbage"), 0o644); err != nil {
		t.Errorf("corrupt index: %v", err)
	}
}

// wfLogEvents returns the msgs of the run's `log` events.
func wfLogEvents(t *testing.T, runID string) []string {
	t.Helper()
	ep, err := subagent.RunEventsPath(runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range readEvents(t, ep) {
		if r.Kind == "log" {
			out = append(out, r.Msg)
		}
	}
	return out
}

// wfBranches lists the repo's cc-fleet snapshot branches.
func wfBranches(t *testing.T, repo string) []string {
	t.Helper()
	out, err := runGit(repo, "branch", "--list", "--format=%(refname:short)", "cc-fleet/wf-*")
	if err != nil {
		t.Fatalf("git branch --list: %v %s", err, out)
	}
	return strings.Fields(out)
}

func wfSamePath(a, b string) bool { return normPath(canonPath(a)) == normPath(canonPath(b)) }

// TestExecuteChdirsToRecordedCwd: Execute runs in the directory the run was launched from, not the
// caller's cwd — the leaves see it and it stays the process cwd for the engine's lifetime.
func TestExecuteChdirsToRecordedCwd(t *testing.T) {
	wfIsolate(t)
	proj, elsewhere := t.TempDir(), t.TempDir()
	var leafCwd string
	wfStubLeaf(t, func(context.Context, subagent.Request) subagent.Result {
		leafCwd, _ = os.Getwd()
		return subagent.Result{OK: true, Result: "ok"}
	})
	_, script := writeScript(t, `await agent("q", {provider: "v"});`)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	run.Cwd = proj
	if err := subagent.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	t.Chdir(elsewhere)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !wfSamePath(leafCwd, proj) {
		t.Errorf("leaf ran in %q, want the recorded run directory %q", leafCwd, proj)
	}
	if cur, _ := os.Getwd(); !wfSamePath(cur, proj) {
		t.Errorf("engine cwd = %q, want %q", cur, proj)
	}
}

// TestExecuteFailsWhenCwdGone: a recorded run directory that can't be entered fails the run before
// any leaf, with a message that tells a vanished directory from an unenterable one.
func TestExecuteFailsWhenCwdGone(t *testing.T) {
	cases := []struct {
		name    string
		mkDir   func(t *testing.T) string
		want    string
		notWant string
	}{
		{"deleted", func(t *testing.T) string {
			d := filepath.Join(t.TempDir(), "proj")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(d); err != nil {
				t.Fatal(err)
			}
			return d
		}, "no longer exists", ""},
		{"permission denied", func(t *testing.T) string {
			if os.Geteuid() == 0 || runtime.GOOS == "windows" {
				t.Skip("needs POSIX directory permissions enforced (non-root unix)")
			}
			d := filepath.Join(t.TempDir(), "proj")
			if err := os.Mkdir(d, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
			return d
		}, "cannot enter run directory", "no longer exists"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wfIsolate(t)
			ran := false
			wfStubLeaf(t, func(context.Context, subagent.Request) subagent.Result {
				ran = true
				return subagent.Result{OK: true}
			})
			_, script := writeScript(t, `await agent("q", {provider: "v"});`)
			run, err := Prepare(script)
			if err != nil {
				t.Fatal(err)
			}
			run.Cwd = c.mkDir(t)
			if err := subagent.SaveRun(run); err != nil {
				t.Fatal(err)
			}
			t.Chdir(t.TempDir())
			err = Execute(context.Background(), script, run.RunID, Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Execute error = %v, want it to contain %q", err, c.want)
			}
			if c.notWant != "" && strings.Contains(err.Error(), c.notWant) {
				t.Errorf("Execute error = %v, must not contain %q", err, c.notWant)
			}
			if ran {
				t.Error("no leaf may run when the run directory can't be entered")
			}
			got, rerr := subagent.ReadRun(run.RunID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if got.Status != "failed" || !strings.Contains(got.Error, c.want) {
				t.Errorf("manifest = {status %q, error %q}, want failed with %q", got.Status, got.Error, c.want)
			}
		})
	}
}

// TestResumePreflightSweepsRecordedRepo: a resume's own-segment sweep targets the repo the run was
// launched from, not the resumer's cwd; a vanished run directory fails the preflight with the
// manifest untouched and nothing swept.
func TestResumePreflightSweepsRecordedRepo(t *testing.T) {
	seed := func(t *testing.T, cwd string) (script, id string, ownCalls *int, gotRoot *string) {
		wfIsolate(t)
		ownCalls, gotRoot = new(int), new(string)
		oldOwn, oldAll := sweepOwnSegmentFn, sweepRunWorktreesFn
		sweepOwnSegmentFn = func(root, _ string) { *ownCalls++; *gotRoot = root }
		sweepRunWorktreesFn = func(string) []string { return nil }
		t.Cleanup(func() { sweepOwnSegmentFn = oldOwn; sweepRunWorktreesFn = oldAll })
		script, _ = writeTrivialScript(t)
		id = "resume-recorded"
		if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe, Cwd: cwd}); err != nil {
			t.Fatal(err)
		}
		t.Chdir(t.TempDir()) // resume from an unrelated, non-git directory
		return script, id, ownCalls, gotRoot
	}

	t.Run("sweeps the recorded repo", func(t *testing.T) {
		repo := initSweepRepo(t)
		script, id, ownCalls, gotRoot := seed(t, repo)
		if _, err := Launch(context.Background(), script, Options{Resume: id}, true); err != nil {
			t.Fatalf("launch resume: %v", err)
		}
		if *ownCalls != 1 {
			t.Fatalf("own-segment sweep fired %d times, want 1", *ownCalls)
		}
		if !wfSamePath(*gotRoot, repo) {
			t.Errorf("own sweep root = %q, want the recorded repo %q", *gotRoot, repo)
		}
	})

	t.Run("recorded directory gone", func(t *testing.T) {
		gone := filepath.Join(t.TempDir(), "gone")
		script, id, ownCalls, _ := seed(t, gone)
		before, err := subagent.ReadRun(id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Launch(context.Background(), script, Options{Resume: id}, true)
		if err == nil || !strings.Contains(err.Error(), "no longer exists") {
			t.Fatalf("resume error = %v, want it to contain %q", err, "no longer exists")
		}
		if *ownCalls != 0 {
			t.Errorf("own-segment sweep fired %d times, want 0", *ownCalls)
		}
		after, err := subagent.ReadRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("the preflight must leave the manifest unchanged:\nbefore %+v\nafter  %+v", before, after)
		}
	})
}

// TestFinishRemovesCleanWorktree: an isolation worktree the leaf didn't change is removed with no
// branch and no log line.
func TestFinishRemovesCleanWorktree(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	const id = "finish-clean"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	t.Chdir(repo)

	wt, finish, err := createWorktree(id)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	if kept := finish("job1", 1); kept != "" {
		t.Errorf("finish on a clean worktree = %q, want \"\"", kept)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("clean worktree dir must be removed (err=%v)", err)
	}
	if worktreeListed(t, repo, wt) {
		t.Error("clean worktree registration must be removed")
	}
	if b := wfBranches(t, repo); len(b) != 0 {
		t.Errorf("a clean worktree must create no branch, got %v", b)
	}
}

// TestFinishKeepsDirtyWorktreeAsBranch (real git): a worktree with leaf changes — uncommitted, or
// committed by the leaf itself — is saved as cc-fleet/wf-<job>-a<attempt> before it is removed; a
// taken branch name gets a random suffix. The user's own checkout is never touched.
func TestFinishKeepsDirtyWorktreeAsBranch(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	const id = "finish-dirty"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	t.Chdir(repo)
	baseHead, _ := runGit(repo, "rev-parse", "HEAD")

	gitOut := func(t *testing.T, dir string, args ...string) string {
		t.Helper()
		out, err := runGit(dir, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	assertSaved := func(t *testing.T, wt, kept, wantBranchRE string) string {
		t.Helper()
		m := regexp.MustCompile(`^branch (` + wantBranchRE + `) \(([0-9a-f]{12})\)$`).FindStringSubmatch(kept)
		if m == nil {
			t.Fatalf("finish = %q, want %q (<commit12>)", kept, wantBranchRE)
		}
		if tip := gitOut(t, repo, "rev-parse", m[1]); !strings.HasPrefix(tip, m[2]) {
			t.Errorf("branch %s = %s, want the reported commit %s", m[1], tip, m[2])
		}
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Errorf("saved worktree dir must be removed (err=%v)", err)
		}
		if worktreeListed(t, repo, wt) {
			t.Error("saved worktree registration must be removed")
		}
		return m[1]
	}

	t.Run("uncommitted changes", func(t *testing.T) {
		wt, finish, err := createWorktree(id)
		if err != nil {
			t.Fatalf("createWorktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("leaf edit"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("new file"), 0o644); err != nil {
			t.Fatal(err)
		}
		branch := assertSaved(t, wt, finish("job1", 2), `cc-fleet/wf-job1-a2`)
		if got := gitOut(t, repo, "show", branch+":f.txt"); got != "leaf edit" {
			t.Errorf("snapshot f.txt = %q, want the leaf's edit", got)
		}
		if got := gitOut(t, repo, "show", branch+":new.txt"); got != "new file" {
			t.Errorf("snapshot new.txt = %q, want the leaf's new file", got)
		}
		msg := gitOut(t, repo, "log", "-1", "--format=%s", branch)
		if want := "cc-fleet workflow snapshot: run " + id + " job job1 attempt 2"; msg != want {
			t.Errorf("snapshot message = %q, want %q", msg, want)
		}
	})

	t.Run("leaf committed itself", func(t *testing.T) {
		wt, finish, err := createWorktree(id)
		if err != nil {
			t.Fatalf("createWorktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wt, "c.txt"), []byte("committed"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, wt, "add", "c.txt")
		gitOut(t, wt, "commit", "-qm", "leaf commit")
		leafHead := gitOut(t, wt, "rev-parse", "HEAD")
		branch := assertSaved(t, wt, finish("job2", 1), `cc-fleet/wf-job2-a1`)
		if tip := gitOut(t, repo, "rev-parse", branch); tip != leafHead {
			t.Errorf("branch %s = %s, want the leaf's own commit %s", branch, tip, leafHead)
		}
	})

	t.Run("branch name taken", func(t *testing.T) {
		gitOut(t, repo, "branch", "cc-fleet/wf-job3-a1")
		wt, finish, err := createWorktree(id)
		if err != nil {
			t.Fatalf("createWorktree: %v", err)
		}
		if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("again"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertSaved(t, wt, finish("job3", 1), `cc-fleet/wf-job3-a1-[0-9a-f]{8}`)
	})

	// The user's checkout: same HEAD, same file, nothing to commit.
	if head, _ := runGit(repo, "rev-parse", "HEAD"); head != baseHead {
		t.Errorf("repo HEAD moved: %s → %s", strings.TrimSpace(baseHead), strings.TrimSpace(head))
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "f.txt")); string(b) != "base" {
		t.Errorf("repo f.txt = %q, want untouched 'base'", b)
	}
	if st := gitOut(t, repo, "status", "--porcelain"); st != "" {
		t.Errorf("repo working tree must stay clean, got:\n%s", st)
	}
}

// TestFinishSnapshotFailureLeavesWorktree: a failed snapshot keeps the worktree with a keep marker,
// and every cleanup path — the resume preflight sweep, the startup sweep and PurgeRun — then spares it
// while still reclaiming a fresh unmarked control worktree placed before each call. None of them
// writes to stderr; the startup sweep and KeptWorktreeNotices report the kept worktree instead.
func TestFinishSnapshotFailureLeavesWorktree(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	const id = "finish-keep" // path-safe
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, id)) })
	t.Chdir(repo)
	// A provably-dead detached run: every cleanup path is allowed to reclaim its segment.
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: 0x7ffffffe}); err != nil {
		t.Fatal(err)
	}

	wt, finish, err := createWorktree(id)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	work := filepath.Join(wt, "work.txt")
	if err := os.WriteFile(work, []byte("unsaved"), 0o644); err != nil {
		t.Fatal(err)
	}
	wfCorruptIndex(t, wt)
	kept := finish("job1", 1)
	if !strings.HasPrefix(kept, "kept at "+wt+": git status: ") {
		t.Fatalf("finish = %q, want a kept-at line for %s", kept, wt)
	}
	marker, err := os.ReadFile(filepath.Join(wt, keepMarkerName))
	if err != nil {
		t.Fatalf("keep marker: %v", err)
	}
	reason := strings.TrimSuffix(string(marker), "\n")
	if strings.Contains(reason, "\n") || reason != strings.TrimPrefix(kept, "kept at "+wt+": ") {
		t.Fatalf("keep marker = %q, want the one-line reason of %q", marker, kept)
	}

	assertKept := func(t *testing.T, step string) {
		t.Helper()
		if b, err := os.ReadFile(work); err != nil || string(b) != "unsaved" {
			t.Errorf("%s: unsaved file = %q (err=%v), want it intact", step, b, err)
		}
		if b, err := os.ReadFile(filepath.Join(wt, keepMarkerName)); err != nil || string(b) != string(marker) {
			t.Errorf("%s: keep marker = %q (err=%v), want %q", step, b, err, marker)
		}
		if !worktreeListed(t, repo, wt) {
			t.Errorf("%s: the kept worktree's registration must stay", step)
		}
	}
	newControl := func(name string) string {
		c := filepath.Join(tempBase, id, name)
		addWorktree(t, repo, c)
		return c
	}
	assertReclaimed := func(t *testing.T, step, ctl string, registration bool) {
		t.Helper()
		if _, err := os.Stat(ctl); !os.IsNotExist(err) {
			t.Errorf("%s: control worktree dir must be removed (err=%v)", step, err)
		}
		if registration && worktreeListed(t, repo, ctl) {
			t.Errorf("%s: control worktree registration must be removed", step)
		}
	}
	hasKept := func(lines []string) bool { // git reports the canonical path, the store listing the raw one
		for _, l := range lines {
			named := strings.Contains(normPath(l), normPath(canonPath(wt))) || strings.Contains(normPath(l), normPath(wt))
			if named && strings.HasPrefix(l, "keeping ") && strings.Contains(l, reason) {
				return true
			}
		}
		return false
	}

	// 1. resume preflight sweep
	ctl1 := newControl("ctl1")
	if out := wfCaptureStderr(t, func() { sweepOwnSegment(repo, id) }); out != "" {
		t.Errorf("sweepOwnSegment wrote to stderr: %q", out)
	}
	assertKept(t, "sweepOwnSegment")
	assertReclaimed(t, "sweepOwnSegment", ctl1, true)

	// 2. startup sweep
	ctl2 := newControl("ctl2")
	var lines []string
	if out := wfCaptureStderr(t, func() { lines = sweepRunWorktrees(repo) }); out != "" {
		t.Errorf("sweepRunWorktrees wrote to stderr: %q", out)
	}
	assertKept(t, "sweepRunWorktrees")
	assertReclaimed(t, "sweepRunWorktrees", ctl2, true)
	if !hasKept(lines) {
		t.Errorf("sweepRunWorktrees = %q, want a line naming the kept worktree", lines)
	}

	// 3. PurgeRun (removes workdirs only; the registration is left to a later sweep)
	ctl3 := newControl("ctl3")
	var perr error
	if out := wfCaptureStderr(t, func() { perr = subagent.PurgeRun(id) }); out != "" {
		t.Errorf("PurgeRun wrote to stderr: %q", out)
	}
	if perr != nil {
		t.Fatalf("PurgeRun: %v", perr)
	}
	if _, err := subagent.ReadRun(id); err == nil {
		t.Error("PurgeRun must remove the manifest")
	}
	assertKept(t, "PurgeRun")
	assertReclaimed(t, "PurgeRun", ctl3, false)

	// 4. a later sweep reclaims the purged control's registration and still spares the kept one
	if out := wfCaptureStderr(t, func() { lines = sweepRunWorktrees(repo) }); out != "" {
		t.Errorf("sweepRunWorktrees (after purge) wrote to stderr: %q", out)
	}
	assertKept(t, "sweepRunWorktrees after PurgeRun")
	assertReclaimed(t, "sweepRunWorktrees after PurgeRun", ctl3, true)
	if !hasKept(lines) {
		t.Errorf("sweepRunWorktrees (after purge) = %q, want a line naming the kept worktree", lines)
	}

	notices := KeptWorktreeNotices(id)
	if len(notices) != 1 || !hasKept(notices) {
		t.Errorf("KeptWorktreeNotices = %q, want exactly the kept worktree", notices)
	}
	if all := KeptWorktreeNotices(""); len(all) != 1 || !hasKept(all) {
		t.Errorf("KeptWorktreeNotices(\"\") = %q, want exactly the kept worktree", all)
	}
}

// TestExecuteLogsKeptWorktree: a marked worktree in the repo is reported by the startup sweep into the
// run's events as a `log` record naming its path, and left in place.
func TestExecuteLogsKeptWorktree(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	const other = "other-run"
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, other)) })
	wt := filepath.Join(tempBase, other, "wt")
	addWorktree(t, repo, wt)
	if err := os.WriteFile(filepath.Join(wt, keepMarkerName), []byte("git status: exit status 128\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script, run := writeTrivialScript(t)
	t.Chdir(repo)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	found := false
	for _, msg := range wfLogEvents(t, run.RunID) {
		if strings.Contains(normPath(msg), normPath(canonPath(wt))) && strings.HasPrefix(msg, "keeping ") {
			found = true
		}
	}
	if !found {
		t.Errorf("events log = %q, want a `keeping %s` line", wfLogEvents(t, run.RunID), wt)
	}
	if _, err := os.Stat(filepath.Join(wt, keepMarkerName)); err != nil {
		t.Errorf("the marked worktree must survive the sweep (err=%v)", err)
	}
}

// TestIsolatedLeafLogsKeptBranch: an isolated leaf that changed its worktree leaves a snapshot branch
// and a `log` event naming it.
func TestIsolatedLeafLogsKeptBranch(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	wfStubLeaf(t, func(_ context.Context, req subagent.Request) subagent.Result {
		_ = os.WriteFile(filepath.Join(req.WorkingDir, "leaf.txt"), []byte("leaf work"), 0o644)
		return subagent.Result{OK: true, Result: "ok"}
	})
	_, script := writeScript(t, `await agent("edit", {provider: "v", isolation: "worktree"});`)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(tempBase, run.RunID)) })
	t.Chdir(repo)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	branches := wfBranches(t, repo)
	if len(branches) != 1 {
		t.Fatalf("snapshot branches = %v, want exactly one", branches)
	}
	found := false
	for _, msg := range wfLogEvents(t, run.RunID) {
		if strings.HasPrefix(msg, "isolation worktree kept: ") && strings.Contains(msg, "branch "+branches[0]+" (") {
			found = true
		}
	}
	if !found {
		t.Errorf("events log = %q, want an isolation-worktree-kept line naming %s", wfLogEvents(t, run.RunID), branches[0])
	}
}

// TestFinishSnapshotAndMarkerFailureWarns (real git, via Execute): when the snapshot fails AND the keep
// marker can't be written, the worktree and its unsaved file are still left in place, and the events
// carry an UNPROTECTED warning naming the path.
func TestFinishSnapshotAndMarkerFailureWarns(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs POSIX directory permissions enforced (non-root unix)")
	}
	repo := initSweepRepo(t)
	wfIsolate(t)
	tempBase := storeWorktreeBase(t)
	var wt string
	wfStubLeaf(t, func(_ context.Context, req subagent.Request) subagent.Result {
		wt = req.WorkingDir
		_ = os.WriteFile(filepath.Join(wt, "work.txt"), []byte("unsaved"), 0o644)
		wfCorruptIndex(t, wt)
		_ = os.Chmod(wt, 0o555)
		return subagent.Result{OK: true, Result: "ok"}
	})
	_, script := writeScript(t, `await agent("edit", {provider: "v", isolation: "worktree"});`)
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wt != "" {
			_ = os.Chmod(wt, 0o755)
		}
		_ = os.RemoveAll(filepath.Join(tempBase, run.RunID))
	})
	t.Chdir(repo)
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if wt == "" {
		t.Fatal("the isolated leaf never ran")
	}
	if b, err := os.ReadFile(filepath.Join(wt, "work.txt")); err != nil || string(b) != "unsaved" {
		t.Errorf("unsaved file = %q (err=%v), want it left in place", b, err)
	}
	if _, err := os.Lstat(filepath.Join(wt, keepMarkerName)); !os.IsNotExist(err) {
		t.Errorf("no keep marker can exist in the read-only worktree (err=%v)", err)
	}
	found := false
	for _, msg := range wfLogEvents(t, run.RunID) {
		if strings.Contains(msg, wt) && strings.Contains(msg, "UNPROTECTED") {
			found = true
		}
	}
	if !found {
		t.Errorf("events log = %q, want an UNPROTECTED line naming %s", wfLogEvents(t, run.RunID), wt)
	}
}
