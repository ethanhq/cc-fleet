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
	"github.com/ethanhq/cc-fleet/internal/ids"
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

// reclaimVerdicts is the reclaim verdict as this package's sweeps see it, held as a seam so a test can
// drive a colliding worktree into the snapshot→verdict window; production is
// subagent.SegmentReclaimVerdicts.
var reclaimVerdicts = subagent.SegmentReclaimVerdicts

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
	storeDir, err := subagent.WorktreeStoreDir()
	if err != nil {
		return "", nil, fmt.Errorf("worktree store dir: %w", err)
	}
	base := filepath.Join(storeDir, ids.WorktreeSegment(runID))
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
// worktrees, keeps only those whose workdir is under THIS store's temp prefix
// (cc-fleet-worktrees/<WorktreeStoreID>), and removes a candidate iff a DEATH PROOF holds — one
// uniform rule, nothing reclaimed on identity alone:
//
//	(a) the segment's verdict (subagent.SegmentReclaimVerdicts) clears — a path-safe owner is provably
//	    engine-dead AND leaf-free (Reclaimer) AND no owner, path-safe or a colliding twin, is alive/
//	    unverifiable or leaf-bearing (!Vetoed). Engine death alone does NOT prove the workdir is unused:
//	    a SIGKILLed engine's claude leaf children (own process groups, cwd = the worktree) outlive it,
//	    and a colliding live twin shares the segment; or
//	(b) the candidate workdir no longer exists on disk — git treats such a registration as prunable,
//	    a live isolation leaf cannot be running in a deleted cwd (so clause (b) needs no leaf check),
//	    and this also reclaims a run whose manifest was already purged out from under a leaked
//	    registration.
//
// The temp prefix is namespaced by config store (WorktreeStoreID), so the VERDICT (clause a) is applied
// only to THIS store's worktrees — a foreign store (another HOME/XDG_CONFIG_HOME) sharing os.TempDir()
// and this repo lives under a DIFFERENT prefix, is verdict-exempt, and can never have its live worktree
// deleted by this store's verdict. A worktree under the shared cc-fleet-worktrees root but outside this
// store's prefix (a foreign store's, or a pre-namespacing flat leftover) is pruned by clause (b) ONLY —
// when its workdir has vanished, which needs no verdict — so present foreign workdirs stay untouched
// while stale registrations don't strand. An UNKNOWN segment under THIS store's prefix whose workdir
// still EXISTS is left alone: it is this store's own run whose manifest is missing/unreadable (a live
// one is spared by the verdict's fail-closed handling), not proof of death. This run's OWN segment gets no privileged reclamation here (an earlier design reclaimed it
// unconditionally, which could delete a blind-stopped-but-live foreground twin's worktrees — the twin
// shares this run's id); a resumed engine's own stale worktrees are reclaimed in the launcher under a
// death proof (sweepOwnSegment). A live foreground engine, a still-running detached engine, and an
// alive-but-unverifiable pid are all left untouched, as are the user's own worktrees (they never live
// under the temp prefix). Segments are ids.WorktreeSegment(id); because that collapses '.'/':'/separators
// to '-' (non-injective: ids "a.b"/"a:b"/"a-b" all yield "a-b"), the reclaim verdict is SEGMENT-level
// (subagent.SegmentReclaimVerdicts): a non-path-safe twin never RECLAIMS (identity reclaim is
// path-safe-only) yet, while alive, always VETOES its segment — so a live "a.b" can't lose its workdirs
// to a dead "a-b", and a dead twin can't provoke reclaiming a live one.
func sweepRunWorktrees(root string) {
	storeID, err := subagent.WorktreeStoreID()
	if err != nil {
		return
	}
	out, err := runGit(root, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	globalPrefix := filepath.Join(canonPath(os.TempDir()), subagent.WorktreeTempName, storeID) + string(os.PathSeparator)
	// flatRoot is the SHARED cc-fleet-worktrees root every store (and any pre-namespacing legacy leftover)
	// sits beneath. A worktree under flatRoot but NOT under globalPrefix belongs to a FOREIGN store or is
	// a legacy flat leftover: this store's verdict says nothing about it, so it is NEVER verdict-reclaimed
	// (that store-local verdict on a foreign present workdir was the r34 deletion hole). It is pruned ONLY
	// when its workdir has vanished — clause (b) needs no verdict (a deleted cwd cannot host a live
	// process, whichever store owned it) — so a stale registration doesn't strand until a manual
	// `git worktree prune`.
	flatRoot := filepath.Join(canonPath(os.TempDir()), subagent.WorktreeTempName) + string(os.PathSeparator)
	// Ownership is keyed by SEGMENT (ids.WorktreeSegment), not exact id, and engine death does NOT prove
	// leaf death (a SIGKILLed engine's claude leaf children — own process groups, cwd = the worktree —
	// outlive it). One ListRuns pass + one leaf scan aggregates, per segment, a Reclaimer (a path-safe
	// dead+leaf-free owner) and a Vetoed flag (ANY owner — path-safe or a colliding twin — alive/
	// unverifiable or leaf-bearing). A scan failure (!verdictsOK) makes every present-workdir reclamation
	// unsafe. The workdir-MISSING clause needs no verdict — a vanished cwd cannot host a running process.
	verdicts, verdictsOK := reclaimVerdicts()
	for _, line := range strings.Split(out, "\n") {
		wt, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !ok {
			continue
		}
		if pathUnder(globalPrefix, wt) {
			seg := runSegment(globalPrefix, wt)
			v := verdicts[seg]
			_, statErr := os.Stat(wt)
			presentReclaimable := verdictsOK && v.Reclaimer && !v.Vetoed
			if presentReclaimable || os.IsNotExist(statErr) {
				removeWorktree(root, wt)
			}
			continue // unknown/live/leaf-bearing present, or a segment a live owner vetoes → kept
		}
		if pathUnder(flatRoot, wt) {
			// A foreign store's or a legacy flat worktree — verdict-exempt: prune ONLY a vanished workdir
			// (clause b), never a present one (it may be a foreign store's live cwd — the r34 hole).
			if _, statErr := os.Stat(wt); os.IsNotExist(statErr) {
				removeWorktree(root, wt)
			}
		}
		// else: not under the cc-fleet temp root at all (the user's own worktree) → left untouched.
	}
}

// sweepOwnSegment reclaims ONLY runID's own isolation-worktree segment in root's .git — every
// registration under cc-fleet-worktrees/<WorktreeStoreID>/<ids.WorktreeSegment(id)>/ plus the segment
// dir. It is the resume launcher's own-stale-worktree cleanup, run there (not in the Execute-time sweep) and only
// after the prior record is confirmed provably dead: the launcher holds the death evidence that the
// manifest rewrite then erases. A non-path-safe id is a no-op — its segment collides with other ids,
// so reclaiming it could delete a twin's live worktrees. Since the launcher already established the
// prior run's death, it adds only the SEGMENT-level veto check: it reclaims unless an owner of the
// segment — the run's own orphan leaf, or a colliding twin — is alive/leaf-bearing. Best-effort.
func sweepOwnSegment(root, runID string) {
	if ids.WorktreeSegment(runID) != runID {
		return // non-path-safe: the segment is shared with colliding ids — never reclaim it
	}
	storeID, err := subagent.WorktreeStoreID()
	if err != nil {
		return
	}
	// Snapshot the segment's registrations BEFORE the verdict — the same ordering sweepRunWorktrees and
	// PurgeRun use. A colliding twin ("a.b", same segment) runs under a DIFFERENT run id, so its run lock
	// never serializes against this sweep; the pre-verdict snapshot is the only guard. A twin that
	// registers a fresh worktree AFTER this list is structurally out of the removal set below, so a stale
	// "reclaimable" verdict can never delete a live owner's just-created worktree.
	out, err := runGit(root, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	// Fail closed: a MISSING entry (no known owner) or a non-reclaimer/vetoed one skips — only a
	// KNOWN path-safe dead+leaf-free owner with no live/colliding twin reclaims.
	verdicts, ok := reclaimVerdicts()
	if v, present := verdicts[runID]; !ok || !present || !v.Reclaimer || v.Vetoed {
		return
	}
	segDir := filepath.Join(canonPath(os.TempDir()), subagent.WorktreeTempName, storeID, runID)
	segPrefix := segDir + string(os.PathSeparator)
	for _, line := range strings.Split(out, "\n") {
		if wt, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree "); ok && pathUnder(segPrefix, wt) {
			removeWorktree(root, wt) // only the pre-verdict snapshot's registrations
		}
	}
	// Remove the segment dir only if now EMPTY — a post-snapshot colliding fresh-uuid workdir (unlisted,
	// so not removed above) keeps it, so os.Remove (not RemoveAll) leaks-not-deletes it.
	_ = os.Remove(segDir)
}

// runSegment returns the run-id path segment (the first component after the store-scoped
// cc-fleet-worktrees/<storeID> prefix) of a worktree path known to be under that prefix.
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
// git repository (worktree isolation requires one). The path is canonicalized (canonPath) so the
// stored root matches the canonical form git porcelain uses — idempotent, since `rev-parse
// --show-toplevel` already resolves symlinks / 8.3 short names, but explicit and platform-uniform.
func gitTopLevel(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("isolation='worktree' requires a git repository (cwd is not one): %v", err)
	}
	return canonPath(strings.TrimSpace(out)), nil
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
