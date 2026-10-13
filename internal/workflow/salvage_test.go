package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

const deadEnginePID = 0x7ffffffe // never a live process in the test env → provably dead

// killedRun models an engine SIGKILLed mid-leaf: a real isolation worktree made by createWorktree
// (its finish never runs) under a provably-dead detached run. Returns the repo and the worktree path.
func killedRun(t *testing.T, id string) (repo, wt string) {
	t.Helper()
	repo = initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, _, err := createWorktree(id) // finish deliberately dropped: the engine was killed
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: deadEnginePID}); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

// dirtyLeaf leaves uncommitted leaf work in wt: an edit and a new file.
func dirtyLeaf(t *testing.T, wt string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("leaf edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("leaf new"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// salvageBranch returns the single cc-fleet/wf-salvage-<id>-* branch in repo.
func salvageBranch(t *testing.T, repo, id string) string {
	t.Helper()
	var got []string
	for _, b := range wfBranches(t, repo) {
		if strings.HasPrefix(b, "cc-fleet/wf-salvage-"+id+"-") {
			got = append(got, b)
		}
	}
	if len(got) != 1 {
		t.Fatalf("salvage branches = %v (all cc-fleet branches %v), want exactly one", got, wfBranches(t, repo))
	}
	return got[0]
}

// gitShow is `git show <rev>:<path>` in repo.
func gitShow(t *testing.T, repo, rev, path string) string {
	t.Helper()
	out, err := runGit(repo, "show", rev+":"+path)
	if err != nil {
		t.Fatalf("git show %s:%s: %v %s", rev, path, err, out)
	}
	return out
}

// assertWorktreeGone asserts wt's workdir and registration are both gone.
func assertWorktreeGone(t *testing.T, repo, wt string) {
	t.Helper()
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir must be removed after salvage (err=%v)", err)
	}
	if worktreeListed(t, repo, wt) {
		t.Error("worktree registration must be removed after salvage")
	}
}

// An engine killed before its finish ran: the death-proof sweep saves the leaf's uncommitted work on
// a cc-fleet/wf-salvage-* branch before deleting the worktree, and reports it in its lines.
func TestSweepSalvagesKilledEngineWork(t *testing.T) {
	const id = "salvage-killed"
	repo, wt := killedRun(t, id)
	dirtyLeaf(t, wt)
	cwt := canonPath(wt) // the form the sweep reports, resolved while wt still exists

	lines := sweepRunWorktrees(repo)

	branch := salvageBranch(t, repo, id)
	if got := gitShow(t, repo, branch, "f.txt"); got != "leaf edit" {
		t.Errorf("salvaged f.txt = %q, want the leaf's edit", got)
	}
	if got := gitShow(t, repo, branch, "new.txt"); got != "leaf new" {
		t.Errorf("salvaged new.txt = %q, want the leaf's new file", got)
	}
	assertWorktreeGone(t, repo, wt)
	if joined := strings.Join(lines, "\n"); !strings.Contains(normPath(joined), normPath("isolation worktree salvaged: "+cwt)) || !strings.Contains(joined, branch) {
		t.Errorf("sweep lines %q must report the salvage of %s onto %s", lines, wt, branch)
	}
}

// The same killed-engine worktree, deleted through PurgeRun (workflow rm / prune): salvaged first.
func TestPurgeRunSalvagesKilledEngineWork(t *testing.T) {
	const id = "salvage-purged"
	repo, wt := killedRun(t, id)
	dirtyLeaf(t, wt)

	if err := subagent.PurgeRun(id); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}

	branch := salvageBranch(t, repo, id)
	if got := gitShow(t, repo, branch, "f.txt"); got != "leaf edit" {
		t.Errorf("salvaged f.txt = %q, want the leaf's edit", got)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("PurgeRun must still delete the salvaged workdir (err=%v)", err)
	}
}

// A leaf that committed inside its worktree (detached HEAD, no ref contains it) is salvaged too.
func TestSweepSalvagesUnreferencedCommit(t *testing.T) {
	const id = "salvage-committed"
	repo, wt := killedRun(t, id)
	dirtyLeaf(t, wt)
	if out, err := runGit(wt, "add", "-A"); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runGit(wt, "commit", "-qm", "leaf commit"); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	head, err := runGit(wt, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	sweepRunWorktrees(repo)

	branch := salvageBranch(t, repo, id)
	if got, _ := runGit(repo, "rev-parse", branch); strings.TrimSpace(got) != strings.TrimSpace(head) {
		t.Errorf("salvage branch at %q, want the leaf's commit %q", strings.TrimSpace(got), strings.TrimSpace(head))
	}
	assertWorktreeGone(t, repo, wt)
}

// A clean worktree still at base is removed, with no branch and no salvage line.
func TestSweepCleanWorktreeNoBranch(t *testing.T) {
	const id = "salvage-clean"
	repo, wt := killedRun(t, id)

	lines := sweepRunWorktrees(repo)

	assertWorktreeGone(t, repo, wt)
	if b := wfBranches(t, repo); len(b) != 0 {
		t.Errorf("a clean worktree must not produce a branch, got %v", b)
	}
	if len(lines) != 0 {
		t.Errorf("a clean removal must not log anything, got %q", lines)
	}
}

// nestedDirty leaves changes the snapshot commit cannot capture: a nested repository with
// uncommitted content (git records only its gitlink), so the tree stays dirty after the commit.
func nestedDirty(t *testing.T, wt string) {
	t.Helper()
	nested := filepath.Join(wt, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		if out, err := runGit(nested, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(nested, "n.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(nested, "add", "."); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runGit(nested, "commit", "-qm", "n"); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(nested, "n.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirtyLeaf(t, wt)
}

// assertWorktreeKept asserts wt survived with its keep marker.
func assertWorktreeKept(t *testing.T, wt string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(wt, "nested", "n.txt")); err != nil || string(b) != "uncommitted" {
		t.Errorf("the nested uncommitted change must survive (got %q, err=%v)", b, err)
	}
	if _, ok := keepMarkerReason(wt); !ok {
		t.Error("a worktree still dirty after the snapshot must carry the keep marker")
	}
}

// Still dirty after the snapshot commit → the sweep keeps the worktree and marks it.
func TestSweepKeepsStillDirtyAfterSnapshot(t *testing.T) {
	const id = "salvage-nested"
	repo, wt := killedRun(t, id)
	nestedDirty(t, wt)

	lines := sweepRunWorktrees(repo)

	assertWorktreeKept(t, wt)
	if !worktreeListed(t, repo, wt) {
		t.Error("a kept worktree's registration must stay")
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(normPath(joined), normPath("keeping "+canonPath(wt))) {
		t.Errorf("sweep lines %q must name the kept worktree", lines)
	}
}

// finishWorktree applies the same post-snapshot check.
func TestFinishKeepsStillDirtyAfterSnapshot(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, finish, err := createWorktree("salvage-finish")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	nestedDirty(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "kept at "+wt) {
		t.Errorf("finish line = %q, want the worktree kept", line)
	}
	assertWorktreeKept(t, wt)
}

// The resume launcher's own-segment sweep salvages too.
func TestSweepOwnSegmentSalvages(t *testing.T) {
	const id = "salvage-own"
	repo, wt := killedRun(t, id)
	dirtyLeaf(t, wt)

	sweepOwnSegment(repo, id)

	branch := salvageBranch(t, repo, id)
	if got := gitShow(t, repo, branch, "new.txt"); got != "leaf new" {
		t.Errorf("salvaged new.txt = %q, want the leaf's new file", got)
	}
	assertWorktreeGone(t, repo, wt)
}
