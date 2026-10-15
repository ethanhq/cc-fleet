package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	createWorktreeFn = func(string) (string, func(string, int) string, error) {
		return "/tmp/fake-wt", func(string, int) string { cleaned = true; return "" }, nil
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

// TestWorktreeRetriesRacingGit: a `git worktree add` or `remove` that fails once, as when it reads
// a registration a parallel leaf is tearing down, is retried — the leaf still gets its worktree and
// nothing stays registered, including a registration whose workdir is already gone (a sweep's case).
func TestWorktreeRetriesRacingGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the git shim is a POSIX shell script")
	}
	for _, c := range []struct {
		name, op string
		gone     bool
	}{{"add", "add", false}, {"remove", "remove", false}, {"remove a vanished workdir", "remove", true}} {
		t.Run(c.name, func(t *testing.T) {
			repo := initSweepRepo(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Chdir(repo)
			var wt, mark string
			if c.op == "add" {
				mark = gitShim(t, "add")
				var finish func(string, int) string
				var err error
				if wt, finish, err = createWorktree("wtretry"); err != nil {
					t.Fatalf("createWorktree after one failed add: %v", err)
				}
				if _, err := os.Stat(filepath.Join(wt, "f.txt")); err != nil {
					t.Fatalf("worktree not checked out: %v", err)
				}
				if kept := finish("job", 1); kept != "" {
					t.Errorf("clean worktree kept: %s", kept)
				}
			} else {
				wt = filepath.Join(t.TempDir(), "wt")
				addWorktree(t, repo, wt)
				if c.gone {
					if err := os.RemoveAll(wt); err != nil {
						t.Fatal(err)
					}
				}
				mark = gitShim(t, "remove")
				removeWorktree(repo, wt)
			}
			if _, err := os.Stat(mark); err != nil {
				t.Errorf("the shim never failed a worktree %s", c.op)
			}
			assertOnlyMainWorktree(t, repo)
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Errorf("workdir %s still there (%v)", wt, err)
			}
		})
	}
}

// TestWorktreeAddHookFailureNotRetried: an add whose checkout finished before a post-checkout hook
// failed is final — adding again would run the hook again — and is cleaned up.
func TestWorktreeAddHookFailureNotRetried(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook is a POSIX shell script")
	}
	repo := initSweepRepo(t)
	hooks, runs := t.TempDir(), filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\necho ran >> \""+runs+"\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runGit(repo, "config", "core.hooksPath", hooks); err != nil {
		t.Fatalf("git config: %v %s", err, out)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Chdir(repo)
	if _, _, err := createWorktree("wthook"); err == nil {
		t.Fatal("createWorktree succeeded despite the failing hook")
	}
	data, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("the post-checkout hook never ran: %v", err)
	}
	if n := strings.Count(string(data), "ran"); n != 1 {
		t.Errorf("post-checkout hook ran %d times, want 1", n)
	}
	assertOnlyMainWorktree(t, repo)
	storeDir, err := subagent.WorktreeStoreDir()
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(storeDir, "*", "*")); len(left) != 0 {
		t.Errorf("workdirs left behind: %v", left)
	}
}

// gitShim puts a git first on PATH that fails the first `git worktree <op>` the way a racing add or
// remove does and passes every other call through to the real git. It returns the marker the shim
// leaves once it has failed.
func gitShim(t *testing.T, op string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, mark := t.TempDir(), filepath.Join(t.TempDir(), "failed")
	shim := "#!/bin/sh\n" +
		`if [ "$1" = worktree ] && [ "$2" = ` + op + ` ] && mkdir "` + mark + `" 2>/dev/null; then` + "\n" +
		`  echo "fatal: failed to read .git/worktrees/x/commondir" >&2; exit 128` + "\n" +
		"fi\n" +
		`exec "` + realGit + `" "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return mark
}

// assertOnlyMainWorktree fails unless `git worktree list` succeeds and shows only repo itself.
func assertOnlyMainWorktree(t *testing.T, repo string) {
	t.Helper()
	out, err := runGit(repo, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v %s", err, out)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d worktrees registered, want only the main one:\n%s", n, out)
	}
}
