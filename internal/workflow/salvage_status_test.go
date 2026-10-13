package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := runGit(dir, args...); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// finishableWorktree makes an isolation worktree through createWorktree in a fresh repo and returns the
// repo, the worktree and its finish.
func finishableWorktree(t *testing.T) (repo, wt string, finish func(string, int) string) {
	t.Helper()
	repo = initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, finish, err := createWorktree("status-finish")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	return repo, wt, finish
}

// leaveNewFile leaves a single uncommitted new file in wt, in a subdirectory so the untracked-dir
// collapse of plain `git status` is exercised too.
func leaveNewFile(t *testing.T, wt string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", "new.txt"), []byte("leaf new"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// leaveIgnoredSubmodule leaves a nested repository with an uncommitted change, declared as a
// submodule with ignore = all, so a plain `git status` hides the change once its gitlink is committed.
func leaveIgnoredSubmodule(t *testing.T, wt string) {
	t.Helper()
	nestedDirty(t, wt)
	gm := "[submodule \"nested\"]\n\tpath = nested\n\turl = ./nested\n\tignore = all\n"
	if err := os.WriteFile(filepath.Join(wt, ".gitmodules"), []byte(gm), 0o644); err != nil {
		t.Fatal(err)
	}
}

// status.showUntrackedFiles=no must not make finish treat a leaf's only new file as a clean tree.
func TestFinishSavesUntrackedWithShowUntrackedNo(t *testing.T) {
	repo, wt, finish := finishableWorktree(t)
	gitIn(t, repo, "config", "status.showUntrackedFiles", "no")
	leaveNewFile(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "branch cc-fleet/wf-job1-a1 ") {
		t.Fatalf("finish line = %q, want the snapshot branch", line)
	}
	if got := gitShow(t, repo, "cc-fleet/wf-job1-a1", "sub/new.txt"); got != "leaf new" {
		t.Errorf("snapshot sub/new.txt = %q, want the leaf's new file", got)
	}
}

// The same config must not make the death-proof sweep delete the new file unsaved.
func TestSweepSalvagesUntrackedWithShowUntrackedNo(t *testing.T) {
	const id = "status-untracked"
	repo, wt := killedRun(t, id)
	gitIn(t, repo, "config", "status.showUntrackedFiles", "no")
	leaveNewFile(t, wt)

	sweepRunWorktrees(repo)

	branch := salvageBranch(t, repo, id)
	if got := gitShow(t, repo, branch, "sub/new.txt"); got != "leaf new" {
		t.Errorf("salvaged sub/new.txt = %q, want the leaf's new file", got)
	}
	assertWorktreeGone(t, repo, wt)
}

// A submodule configured ignore = all with uncommitted changes inside must keep finish's worktree.
func TestFinishKeepsIgnoredSubmoduleChanges(t *testing.T) {
	_, wt, finish := finishableWorktree(t)
	leaveIgnoredSubmodule(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "kept at "+wt) {
		t.Errorf("finish line = %q, want the worktree kept", line)
	}
	assertWorktreeKept(t, wt)
}

// The same submodule under the sweep: salvage keeps and marks the worktree.
func TestSweepKeepsIgnoredSubmoduleChanges(t *testing.T) {
	const id = "status-submodule"
	repo, wt := killedRun(t, id)
	leaveIgnoredSubmodule(t, wt)

	sweepRunWorktrees(repo)

	assertWorktreeKept(t, wt)
	if !worktreeListed(t, repo, wt) {
		t.Error("a kept worktree's registration must stay")
	}
}
