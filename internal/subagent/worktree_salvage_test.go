package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fxwfGit runs git in dir, failing the test on error.
func fxwfGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := salvageGit(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(out)
}

// fxwfWorktree makes a committed repo plus a detached worktree at <tmp>/<seg>/<dir> and returns both.
func fxwfWorktree(t *testing.T, seg, dir string) (repo, wt string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	fxwfGit(t, repo, "init", "-q")
	fxwfGit(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init")
	wt = filepath.Join(t.TempDir(), seg, dir)
	fxwfGit(t, repo, "worktree", "add", "-q", "--detach", wt, "HEAD")
	return repo, wt
}

func TestFxwfSalvageWorktreeNotAWorktree(t *testing.T) {
	d := t.TempDir()
	if note, remove := SalvageWorktree(d); !remove || note != "" {
		t.Errorf("a dir with no .git = (%q, %v), want removable with no note", note, remove)
	}
	if _, err := os.Stat(filepath.Join(d, worktreeKeepMarker)); !os.IsNotExist(err) {
		t.Errorf("no keep marker may be written into a non-worktree dir (err=%v)", err)
	}
	if note, remove := SalvageWorktree(filepath.Join(d, "missing")); !remove || note != "" {
		t.Errorf("a missing path = (%q, %v), want removable with no note", note, remove)
	}
}

func TestFxwfSalvageWorktreeStatusFailureKeeps(t *testing.T) {
	_, wt := fxwfWorktree(t, "seg", "0123456789")
	gd := fxwfGit(t, wt, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gd, "index"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, remove := SalvageWorktree(wt)
	if remove || !strings.HasPrefix(note, "git status") {
		t.Errorf("SalvageWorktree = (%q, %v), want kept with the git status failure", note, remove)
	}
	if b, err := os.ReadFile(filepath.Join(wt, worktreeKeepMarker)); err != nil || strings.TrimSpace(string(b)) != note {
		t.Errorf("keep marker = %q (err=%v), want the reason %q", b, err, note)
	}
}

func TestFxwfSalvageWorktreeBranchCollisionRetries(t *testing.T) {
	repo, wt := fxwfWorktree(t, "seg", "0123456789")
	fxwfGit(t, repo, "branch", "cc-fleet/wf-salvage-seg-01234567") // the name is taken
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, remove := SalvageWorktree(wt)
	if !remove || !strings.HasPrefix(note, "branch cc-fleet/wf-salvage-seg-01234567-") {
		t.Fatalf("SalvageWorktree = (%q, %v), want a suffixed salvage branch", note, remove)
	}
	branch := strings.Fields(note)[1]
	if got := fxwfGit(t, repo, "show", branch+":f.txt"); got != "leaf" {
		t.Errorf("salvaged f.txt = %q, want %q", got, "leaf")
	}
}
