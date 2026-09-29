package subagent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
)

// worktreeKeepMarker marks an isolation worktree whose work could not be saved: the workflow sweeps and
// PurgeRun skip a worktree holding it. Mirrors the workflow package's keepMarkerName.
const worktreeKeepMarker = ".cc-fleet-keep"

// SalvageWorktree saves an isolation worktree's work before a reclaim deletes it — needed because a
// killed engine never runs the worktree's finish. Uncommitted changes are committed as a snapshot, and a
// HEAD that no ref contains is kept as branch cc-fleet/wf-salvage-<segment>-<dir[:8]>. remove reports
// whether wt may now be deleted; note then names the branch, or is "" when nothing needed saving. When a
// step fails, or the tree is still dirty after the snapshot (e.g. changes inside a nested repository), a
// keep marker holding the reason is written, remove is false and note is the reason. A path with no .git
// entry is not a worktree (git would act on an enclosing repository), so there is nothing to save.
func SalvageWorktree(wt string) (note string, remove bool) {
	fi, err := os.Lstat(wt)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
		return "", true
	}
	if err == nil {
		_, err = os.Lstat(filepath.Join(wt, ".git"))
		if errors.Is(err, fs.ErrNotExist) {
			return "", true
		}
	}
	if err != nil {
		return salvageKeep(wt, err.Error())
	}
	// Explicit flags: status.showUntrackedFiles or a submodule ignore setting must not hide leaf work.
	status, err := salvageGit(wt, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return salvageKeep(wt, salvageErr("status", err, status))
	}
	if strings.TrimSpace(status) != "" {
		if out, err := salvageGit(wt, "add", "-A"); err != nil {
			return salvageKeep(wt, salvageErr("add", err, out))
		}
		if out, err := salvageGit(wt, "-c", "user.name=cc-fleet", "-c", "user.email=cc-fleet@localhost", "-c", "commit.gpgsign=false",
			"commit", "--no-verify", "-q", "-m", "cc-fleet workflow salvage snapshot"); err != nil {
			return salvageKeep(wt, salvageErr("commit", err, out))
		}
		if status, err = salvageGit(wt, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=none"); err != nil {
			return salvageKeep(wt, salvageErr("status", err, status))
		}
		if strings.TrimSpace(status) != "" {
			return salvageKeep(wt, "changes remain after the snapshot commit (e.g. inside a nested repository or submodule)")
		}
	}
	contains, err := salvageGit(wt, "for-each-ref", "--contains", "HEAD", "--count=1", "--format=%(refname)")
	if err != nil {
		return salvageKeep(wt, salvageErr("for-each-ref", err, contains))
	}
	if strings.TrimSpace(contains) != "" {
		return "", true
	}
	head, err := salvageGit(wt, "rev-parse", "HEAD")
	if err != nil {
		return salvageKeep(wt, salvageErr("rev-parse HEAD", err, head))
	}
	head = strings.TrimSpace(head)
	dir := filepath.Base(wt)
	branch := fmt.Sprintf("cc-fleet/wf-salvage-%s-%s", filepath.Base(filepath.Dir(wt)), dir[:min(8, len(dir))])
	if _, err := salvageGit(wt, "branch", branch, head); err != nil {
		branch += "-" + uuid.NewString()[:8] // the name is most likely taken: retry once with a random suffix
		if out, err := salvageGit(wt, "branch", branch, head); err != nil {
			return salvageKeep(wt, salvageErr("branch", err, out))
		}
	}
	return fmt.Sprintf("branch %s (%s)", branch, head[:min(12, len(head))]), true
}

// salvageKeep writes wt's keep marker with reason and reports wt as not removable.
func salvageKeep(wt, reason string) (string, bool) {
	if err := os.WriteFile(filepath.Join(wt, worktreeKeepMarker), []byte(reason+"\n"), 0o600); err != nil {
		return fmt.Sprintf("%s (keep marker not written: %v)", reason, err), false
	}
	return reason, false
}

// salvageErr is a one-line summary of a failed git step: the error plus git's first output line.
func salvageErr(step string, err error, out string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if first = strings.TrimSpace(first); first == "" {
		return fmt.Sprintf("git %s: %v", step, err)
	}
	return fmt.Sprintf("git %s: %v: %s", step, err, first)
}

// salvageGit runs git in dir with a cred-scrubbed env and returns combined output. Unbounded, unlike
// runGitBounded: a snapshot of a large tree may take longer than its deadline.
func salvageGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = childenv.Clean(os.Environ())
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
