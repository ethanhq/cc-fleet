package workflow

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// createWorktreeFn is a seam so tests inject a fake worktree (no real git) — production is
// createWorktree. It returns the worktree path, a cleanup func, and an error.
var createWorktreeFn = createWorktree

// sweepRunWorktreesFn / sweepOwnSegmentFn are seams so tests assert the Execute-time and resume-
// launcher sweeps fire without real git — production is sweepRunWorktrees / sweepOwnSegment.
var (
	sweepRunWorktreesFn = sweepRunWorktrees
	sweepOwnSegmentFn   = sweepOwnSegment
)

// createWorktree makes a fresh detached `git worktree` from the run's repo (cwd's repo)
// at HEAD, under a run-scoped temp root, and returns its path + a cleanup. A leaf run with
// cwd = this worktree edits an isolated copy, so parallel file-editing leaves don't
// collide. The worktree is removed on cleanup (deferred by the caller on done/fail/panic).
// The git registration lands in the user's repo .git; a graceful path removes it at once,
// while an engine SIGKILL/crash skips the deferred cleanup and leaves a stale registration —
// reclaimed later under a death proof: the next engine's startup sweepRunWorktrees (a
// provably-dead detached run, or any vanished workdir), or a resume's own-segment sweep.
// Cross-platform via the git CLI (no cgo). A non-git cwd is a clear error. The git child env
// is scrubbed of creds (childenv.Clean) — git needs none.
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
		removeWorktree(root, wt) // a partial add may have left a registration and/or a dir
		return "", nil, fmt.Errorf("git worktree add: %v: %s", gerr, strings.TrimSpace(out))
	}
	cleanup := func() { removeWorktree(root, wt) }
	return wt, cleanup, nil
}

// removeWorktree unregisters wt from root's .git and deletes its workdir, best-effort. The
// RemoveAll is belt-and-braces: `worktree remove --force` normally clears the dir, but a
// partial add or a stray file can outlive it; a git error just means nothing was registered.
func removeWorktree(root, wt string) {
	_, _ = runGit(root, "worktree", "remove", "--force", wt)
	_ = os.RemoveAll(wt)
}

// sweepRunWorktrees reclaims stale isolation-worktree registrations left in root's .git by
// crashed/SIGKILLed engines (a bare SIGKILL skips the deferred cleanup, so both the git
// registration and the temp workdir leak). Best-effort, no error return. It lists root's
// worktrees, keeps only those whose workdir is under the global cc-fleet-worktrees temp prefix,
// and removes a candidate iff a DEATH PROOF holds — one uniform rule, nothing reclaimed on
// identity alone:
//
//	(a) the segment is KNOWN to this config store's ListRuns AND its owning run is provably dead
//	    (RunEngineProvablyNotLive); or
//	(b) the candidate workdir no longer exists on disk — git treats such a registration as prunable,
//	    a live isolation leaf cannot be running in a deleted cwd, and this also reclaims a run whose
//	    manifest was already purged out from under a leaked registration.
//
// An UNKNOWN segment whose workdir still EXISTS is left alone: absence from THIS store's list is
// not proof of death — a different config store (another HOME/XDG_CONFIG_HOME) may own a live run
// this process cannot see. This run's OWN segment gets no privileged reclamation here (an earlier
// design reclaimed it unconditionally, which could delete a blind-stopped-but-live foreground
// twin's worktrees — the twin shares this run's id); a resumed engine's own stale worktrees are
// reclaimed in the launcher under a death proof (sweepOwnSegment). A live foreground engine, a
// still-running detached engine, and an alive-but-unverifiable pid are all left untouched, as are
// the user's own worktrees (they never live under the temp prefix). Segments are sanitizeRunID(id);
// because that collapses '.'/':'/separators to '-' (non-injective: ids "a.b"/"a:b"/"a-b" all yield
// "a-b"), only PATH-SAFE ids populate deadBySegment, keeping it injective so a dead twin can't
// last-writer-wins overwrite a live run's liveness.
func sweepRunWorktrees(root string) {
	out, err := runGit(root, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	globalPrefix := filepath.Join(os.TempDir(), "cc-fleet-worktrees") + string(os.PathSeparator)
	deadBySegment := map[string]bool{}
	if runs, lerr := subagent.ListRuns(); lerr == nil {
		for _, r := range runs {
			if sanitizeRunID(r.RunID) == r.RunID { // path-safe → equals its own segment
				deadBySegment[r.RunID] = subagent.RunEngineProvablyNotLive(r)
			}
		}
	}
	for _, line := range strings.Split(out, "\n") {
		wt, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !ok {
			continue
		}
		if !pathUnder(globalPrefix, wt) {
			continue
		}
		seg := runSegment(globalPrefix, wt)
		dead, known := deadBySegment[seg]
		_, statErr := os.Stat(wt)
		remove := (known && dead) || os.IsNotExist(statErr)
		if !remove {
			continue // unknown-but-present, or a known-live run — another store may own it
		}
		removeWorktree(root, wt)
	}
}

// sweepOwnSegment reclaims ONLY runID's own isolation-worktree segment in root's .git — every
// registration under cc-fleet-worktrees/<sanitizeRunID(id)>/ plus the segment dir. It is the resume
// launcher's own-stale-worktree cleanup, run there (not in the Execute-time sweep) and only after
// the prior record is confirmed provably dead: the launcher holds the death evidence that the
// manifest rewrite then erases. A non-path-safe id is a no-op — its segment collides with other ids
// under sanitizeRunID, so reclaiming it could delete a twin's live worktrees (the guard PurgeRun and
// deadBySegment apply). Best-effort, no error return.
func sweepOwnSegment(root, runID string) {
	if sanitizeRunID(runID) != runID {
		return // non-path-safe: the segment is shared with colliding ids — never reclaim it
	}
	segDir := filepath.Join(os.TempDir(), "cc-fleet-worktrees", runID)
	if out, err := runGit(root, "worktree", "list", "--porcelain"); err == nil {
		segPrefix := segDir + string(os.PathSeparator)
		for _, line := range strings.Split(out, "\n") {
			if wt, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok && pathUnder(segPrefix, wt) {
				removeWorktree(root, wt)
			}
		}
	}
	_ = os.RemoveAll(segDir)
}

// runSegment returns the run-id path segment (the first component after the global
// cc-fleet-worktrees prefix) of a worktree path known to be under that prefix.
func runSegment(globalPrefix, wt string) string {
	rest := strings.TrimPrefix(normPath(wt), normPath(globalPrefix))
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}

// pathUnder reports whether wt is under prefix. Both sides are normalized (forward slashes,
// and case-folded on Windows, where git porcelain emits forward-slash paths while os.TempDir
// yields backslashes) so the comparison holds cross-platform.
func pathUnder(prefix, wt string) bool {
	return strings.HasPrefix(normPath(wt), normPath(prefix))
}

// normPath forward-slashes a path and, on Windows only, lower-cases it, so a case- and
// separator-insensitive prefix comparison is correct on each platform.
func normPath(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
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
