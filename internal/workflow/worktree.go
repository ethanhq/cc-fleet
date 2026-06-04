package workflow

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
)

// createWorktreeFn is a seam so tests inject a fake worktree (no real git) — production is
// createWorktree. It returns the worktree path, a cleanup func, and an error.
var createWorktreeFn = createWorktree

// createWorktree makes a fresh detached `git worktree` from the run's repo (cwd's repo)
// at HEAD, under a run-scoped temp root, and returns its path + a cleanup. A leaf run with
// cwd = this worktree edits an isolated copy, so parallel file-editing leaves don't
// collide. The worktree is removed on cleanup (deferred by the caller on done/fail/panic);
// the run-scoped root means an engine SIGKILL leaves at most that run's worktrees on a
// temp filesystem, not the user's repo. Cross-platform via the git CLI (no cgo). A non-git
// cwd is a clear error. The git child env is scrubbed of creds (childenv.Clean) — git
// needs none.
func createWorktree(runID string) (string, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	root, err := gitTopLevel(cwd)
	if err != nil {
		return "", nil, err
	}
	base := filepath.Join(os.TempDir(), "cc-fleet-worktrees", sanitizeRunID(runID))
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, fmt.Errorf("worktree base: %w", err)
	}
	wt := filepath.Join(base, uuid.NewString())
	if out, gerr := runGit(root, "worktree", "add", "--detach", wt, "HEAD"); gerr != nil {
		// A partial `worktree add` can leave a registered worktree and/or a dir behind;
		// clean both up best-effort so a failure doesn't leak.
		_, _ = runGit(root, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
		return "", nil, fmt.Errorf("git worktree add: %v: %s", gerr, strings.TrimSpace(out))
	}
	cleanup := func() {
		_, _ = runGit(root, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt) // belt-and-braces if `worktree remove` left anything
	}
	return wt, cleanup, nil
}

// gitTopLevel returns the repo root containing dir, or a clear error when dir is not in a
// git repository (worktree isolation requires one).
func gitTopLevel(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("isolation='worktree' requires a git repository (cwd is not one): %v", err)
	}
	return strings.TrimSpace(out), nil
}

// runGit runs a git command in dir with a cred-scrubbed env and returns combined output.
func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = childenv.Clean(os.Environ())
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// sanitizeRunID keeps a run id safe as a single path segment for the temp worktree root.
func sanitizeRunID(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '.' || r == ':' {
			return '-'
		}
		return r
	}, id)
}
