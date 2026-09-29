package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

const fxwfDeadPID = 0x7ffffffe // never a live process in the test env → provably dead

// fxwfKilledRun models an engine SIGKILLed mid-leaf: a real isolation worktree made by createWorktree
// (its finish never runs) under a provably-dead detached run. Returns the repo and the worktree path.
func fxwfKilledRun(t *testing.T, id string) (repo, wt string) {
	t.Helper()
	repo = initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, _, err := createWorktree(id) // finish deliberately dropped: the engine was killed
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	if err := subagent.SaveRun(subagent.WorkflowRun{RunID: id, StartedAt: "2026-01-01T00:00:00Z", Status: "stopped", EnginePID: fxwfDeadPID}); err != nil {
		t.Fatal(err)
	}
	return repo, wt
}

// fxwfDirtyLeaf leaves uncommitted leaf work in wt: an edit and a new file.
func fxwfDirtyLeaf(t *testing.T, wt string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("leaf edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("leaf new"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fxwfSalvageBranch returns the single cc-fleet/wf-salvage-<id>-* branch in repo.
func fxwfSalvageBranch(t *testing.T, repo, id string) string {
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

// fxwfShow is `git show <rev>:<path>` in repo.
func fxwfShow(t *testing.T, repo, rev, path string) string {
	t.Helper()
	out, err := runGit(repo, "show", rev+":"+path)
	if err != nil {
		t.Fatalf("git show %s:%s: %v %s", rev, path, err, out)
	}
	return out
}

// fxwfAssertGone asserts wt's workdir and registration are both gone.
func fxwfAssertGone(t *testing.T, repo, wt string) {
	t.Helper()
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir must be removed after salvage (err=%v)", err)
	}
	if worktreeListed(t, repo, wt) {
		t.Error("worktree registration must be removed after salvage")
	}
}

// (a) An engine killed before its finish ran: the death-proof sweep saves the leaf's uncommitted work on
// a cc-fleet/wf-salvage-* branch before deleting the worktree, and reports it in its lines.
func TestFxwfSweepSalvagesKilledEngineWork(t *testing.T) {
	const id = "fxwf-killed"
	repo, wt := fxwfKilledRun(t, id)
	fxwfDirtyLeaf(t, wt)
	cwt := canonPath(wt) // the form the sweep reports, resolved while wt still exists

	lines := sweepRunWorktrees(repo)

	branch := fxwfSalvageBranch(t, repo, id)
	if got := fxwfShow(t, repo, branch, "f.txt"); got != "leaf edit" {
		t.Errorf("salvaged f.txt = %q, want the leaf's edit", got)
	}
	if got := fxwfShow(t, repo, branch, "new.txt"); got != "leaf new" {
		t.Errorf("salvaged new.txt = %q, want the leaf's new file", got)
	}
	fxwfAssertGone(t, repo, wt)
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "isolation worktree salvaged: "+cwt) || !strings.Contains(joined, branch) {
		t.Errorf("sweep lines %q must report the salvage of %s onto %s", lines, wt, branch)
	}
}

// (b) The same killed-engine worktree, deleted through PurgeRun (workflow rm / prune): salvaged first.
func TestFxwfPurgeRunSalvagesKilledEngineWork(t *testing.T) {
	const id = "fxwf-purged"
	repo, wt := fxwfKilledRun(t, id)
	fxwfDirtyLeaf(t, wt)

	if err := subagent.PurgeRun(id); err != nil {
		t.Fatalf("PurgeRun: %v", err)
	}

	branch := fxwfSalvageBranch(t, repo, id)
	if got := fxwfShow(t, repo, branch, "f.txt"); got != "leaf edit" {
		t.Errorf("salvaged f.txt = %q, want the leaf's edit", got)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("PurgeRun must still delete the salvaged workdir (err=%v)", err)
	}
}

// (c) A leaf that committed inside its worktree (detached HEAD, no ref contains it) is salvaged too.
func TestFxwfSweepSalvagesUnreferencedCommit(t *testing.T) {
	const id = "fxwf-committed"
	repo, wt := fxwfKilledRun(t, id)
	fxwfDirtyLeaf(t, wt)
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

	branch := fxwfSalvageBranch(t, repo, id)
	if got, _ := runGit(repo, "rev-parse", branch); strings.TrimSpace(got) != strings.TrimSpace(head) {
		t.Errorf("salvage branch at %q, want the leaf's commit %q", strings.TrimSpace(got), strings.TrimSpace(head))
	}
	fxwfAssertGone(t, repo, wt)
}

// (d) A clean worktree still at base is removed as before, with no branch and no salvage line.
func TestFxwfSweepCleanWorktreeNoBranch(t *testing.T) {
	const id = "fxwf-clean"
	repo, wt := fxwfKilledRun(t, id)

	lines := sweepRunWorktrees(repo)

	fxwfAssertGone(t, repo, wt)
	if b := wfBranches(t, repo); len(b) != 0 {
		t.Errorf("a clean worktree must not produce a branch, got %v", b)
	}
	if len(lines) != 0 {
		t.Errorf("a clean removal must not log anything, got %q", lines)
	}
}

// fxwfNestedDirty leaves changes the snapshot commit cannot capture: a nested repository with
// uncommitted content (git records only its gitlink), so the tree stays dirty after the commit.
func fxwfNestedDirty(t *testing.T, wt string) {
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
	fxwfDirtyLeaf(t, wt)
}

// fxwfAssertKept asserts wt survived with its keep marker.
func fxwfAssertKept(t *testing.T, wt string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(wt, "nested", "n.txt")); err != nil || string(b) != "uncommitted" {
		t.Errorf("the nested uncommitted change must survive (got %q, err=%v)", b, err)
	}
	if _, ok := keepMarkerReason(wt); !ok {
		t.Error("a worktree still dirty after the snapshot must carry the keep marker")
	}
}

// (e) Still dirty after the snapshot commit → the sweep keeps the worktree and marks it.
func TestFxwfSweepKeepsStillDirtyAfterSnapshot(t *testing.T) {
	const id = "fxwf-nested"
	repo, wt := fxwfKilledRun(t, id)
	fxwfNestedDirty(t, wt)

	lines := sweepRunWorktrees(repo)

	fxwfAssertKept(t, wt)
	if !worktreeListed(t, repo, wt) {
		t.Error("a kept worktree's registration must stay")
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "keeping "+canonPath(wt)) {
		t.Errorf("sweep lines %q must name the kept worktree", lines)
	}
}

// (e) finishWorktree applies the same post-snapshot check.
func TestFxwfFinishKeepsStillDirtyAfterSnapshot(t *testing.T) {
	repo := initSweepRepo(t)
	wfIsolate(t)
	t.Chdir(repo)
	wt, finish, err := createWorktree("fxwf-finish")
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(wt)) })
	fxwfNestedDirty(t, wt)

	line := finish("job1", 1)

	if !strings.HasPrefix(line, "kept at "+wt) {
		t.Errorf("finish line = %q, want the worktree kept", line)
	}
	fxwfAssertKept(t, wt)
}

// The resume launcher's own-segment sweep salvages too.
func TestFxwfSweepOwnSegmentSalvages(t *testing.T) {
	const id = "fxwf-own"
	repo, wt := fxwfKilledRun(t, id)
	fxwfDirtyLeaf(t, wt)

	sweepOwnSegment(repo, id)

	branch := fxwfSalvageBranch(t, repo, id)
	if got := fxwfShow(t, repo, branch, "new.txt"); got != "leaf new" {
		t.Errorf("salvaged new.txt = %q, want the leaf's new file", got)
	}
	fxwfAssertGone(t, repo, wt)
}
