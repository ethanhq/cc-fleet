package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestWorktreeIsolationWiring (seam): isolation: 'worktree' creates a worktree, runs the
// leaf with cwd = its path, and tears it down after — via the createWorktreeFn seam (no
// real git needed).
func TestWorktreeIsolationWiring(t *testing.T) {
	rec := &recorder{}
	var gotDir string
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		gotDir = c.workingDir
		return subagent.Result{OK: true, Result: "ok"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	cleaned := false
	oldW := createWorktreeFn
	createWorktreeFn = func(string) (string, func(), error) {
		return "/tmp/fake-wt", func() { cleaned = true }, nil
	}
	t.Cleanup(func() { createWorktreeFn = oldW })

	if _, err := runScript(t, "wt", 2, leaf, `await agent("edit", {provider: "v", isolation: "worktree"});`); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotDir != "/tmp/fake-wt" {
		t.Errorf("leaf WorkingDir = %q, want /tmp/fake-wt", gotDir)
	}
	if !cleaned {
		t.Error("the worktree cleanup must run after the leaf")
	}
}

// TestWorktreeRejections: a bad isolation value is an error, and run_in_background is not
// an option (background = an unawaited agent() promise) — passing it fails loudly.
func TestWorktreeRejections(t *testing.T) {
	rec := &recorder{}
	if _, err := runScript(t, "wtb1", 2, echoLeaf(rec),
		`return await agent("q", {provider: "v", isolation: "docker"});`); err == nil || !strings.Contains(err.Error(), "isolation must be") {
		t.Errorf("bad isolation value should error, got %v", err)
	}
	if _, err := runScript(t, "wtb2", 2, echoLeaf(rec),
		`return await agent("q", {provider: "v", isolation: "worktree", run_in_background: true});`); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Errorf("run_in_background should be rejected as an unknown option, got %v", err)
	}
}

// TestWorktreeRealGit (acceptance): two parallel worktree leaves editing the SAME path get
// distinct worktrees (no collision), the main repo file is untouched, and the worktrees are
// cleaned up. Skipped where git is unavailable.
func TestWorktreeRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"},
	} {
		if out, err := runGit(repo, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	shared := filepath.Join(repo, "shared.txt")
	if err := os.WriteFile(shared, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(repo, "add", "."); err != nil {
		t.Fatalf("git add: %v %s", err, out)
	}
	if out, err := runGit(repo, "commit", "-qm", "init"); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	t.Chdir(repo) // createWorktree resolves the repo from cwd

	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		// Each leaf writes the SAME relative path inside its own worktree.
		_ = os.WriteFile(filepath.Join(c.workingDir, "shared.txt"), []byte("leaf:"+c.prompt), 0o644)
		return subagent.Result{OK: true, Result: c.workingDir}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	g, err := runScript(t, "wtgit", 2, leaf, `
const r = await parallel([
    () => agent("a", {provider: "v", isolation: "worktree"}),
    () => agent("b", {provider: "v", isolation: "worktree"}),
]);
return { r };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := listField(t, wantMap(t, g), "r")
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	d0, d1 := strAt(t, got, 0), strAt(t, got, 1)
	if d0 == d1 || d0 == "" || d1 == "" {
		t.Errorf("worktree leaves must get DISTINCT worktrees, got %q and %q", d0, d1)
	}
	// The main repo's file is untouched (the edits were isolated to the worktrees).
	if b, _ := os.ReadFile(shared); string(b) != "base" {
		t.Errorf("main repo shared.txt = %q, want untouched 'base'", b)
	}
	// Worktrees are cleaned up: `git worktree list` shows only the main worktree.
	if out, _ := runGit(repo, "worktree", "list"); strings.Count(out, "\n") > 1 {
		t.Errorf("worktrees not cleaned up:\n%s", out)
	}
}
