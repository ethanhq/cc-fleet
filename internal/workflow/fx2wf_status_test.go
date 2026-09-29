package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fx2wfGit runs git in dir, failing the test on error.
func fx2wfGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := runGit(dir, args...); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// fx2wfFinishWorktree makes an isolation worktree through createWorktree in a fresh repo and returns the
// repo, the worktree and its finish.
func fx2wfFinishWorktree(t *testing.T) (repo, wt string, finish func(string, int) string) {
	t.Helper()
	repo = initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, finish, err := createWorktree("fx2wf-finish")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	return repo, wt, finish
}

// fx2wfNewFileOnly leaves a single uncommitted new file in wt, in a subdirectory so the untracked-dir
// collapse of plain `git status` is exercised too.
func fx2wfNewFileOnly(t *testing.T, wt string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", "new.txt"), []byte("leaf new"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fx2wfIgnoredSubmoduleDirty leaves a nested repository with an uncommitted change, declared as a
// submodule with ignore = all, so a plain `git status` hides the change once its gitlink is committed.
func fx2wfIgnoredSubmoduleDirty(t *testing.T, wt string) {
	t.Helper()
	fxwfNestedDirty(t, wt)
	gm := "[submodule \"nested\"]\n\tpath = nested\n\turl = ./nested\n\tignore = all\n"
	if err := os.WriteFile(filepath.Join(wt, ".gitmodules"), []byte(gm), 0o644); err != nil {
		t.Fatal(err)
	}
}

// status.showUntrackedFiles=no must not make finish treat a leaf's only new file as a clean tree.
func TestFx2wfFinishSavesUntrackedWithShowUntrackedNo(t *testing.T) {
	repo, wt, finish := fx2wfFinishWorktree(t)
	fx2wfGit(t, repo, "config", "status.showUntrackedFiles", "no")
	fx2wfNewFileOnly(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "branch cc-fleet/wf-job1-a1 ") {
		t.Fatalf("finish line = %q, want the snapshot branch", line)
	}
	if got := fxwfShow(t, repo, "cc-fleet/wf-job1-a1", "sub/new.txt"); got != "leaf new" {
		t.Errorf("snapshot sub/new.txt = %q, want the leaf's new file", got)
	}
}

// The same config must not make the death-proof sweep delete the new file unsaved.
func TestFx2wfSweepSalvagesUntrackedWithShowUntrackedNo(t *testing.T) {
	const id = "fx2wf-untracked"
	repo, wt := fxwfKilledRun(t, id)
	fx2wfGit(t, repo, "config", "status.showUntrackedFiles", "no")
	fx2wfNewFileOnly(t, wt)

	sweepRunWorktrees(repo)

	branch := fxwfSalvageBranch(t, repo, id)
	if got := fxwfShow(t, repo, branch, "sub/new.txt"); got != "leaf new" {
		t.Errorf("salvaged sub/new.txt = %q, want the leaf's new file", got)
	}
	fxwfAssertGone(t, repo, wt)
}

// A submodule configured ignore = all with uncommitted changes inside must keep finish's worktree.
func TestFx2wfFinishKeepsIgnoredSubmoduleChanges(t *testing.T) {
	_, wt, finish := fx2wfFinishWorktree(t)
	fx2wfIgnoredSubmoduleDirty(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "kept at "+wt) {
		t.Errorf("finish line = %q, want the worktree kept", line)
	}
	fxwfAssertKept(t, wt)
}

// The same submodule under the sweep: salvage keeps and marks the worktree.
func TestFx2wfSweepKeepsIgnoredSubmoduleChanges(t *testing.T) {
	const id = "fx2wf-submodule"
	repo, wt := fxwfKilledRun(t, id)
	fx2wfIgnoredSubmoduleDirty(t, wt)

	sweepRunWorktrees(repo)

	fxwfAssertKept(t, wt)
	if !worktreeListed(t, repo, wt) {
		t.Error("a kept worktree's registration must stay")
	}
}
