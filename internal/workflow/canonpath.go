//go:build !windows

package workflow

import "path/filepath"

// canonPath resolves p to the form `git worktree list --porcelain` / `git rev-parse --show-toplevel`
// emit, so a prefix comparison against git output holds on every platform. On unix/darwin git reports
// the symlink-resolved path (notably macOS, where os.TempDir() is /var/folders/… but git canonicalizes
// the /var symlink to /private/var/folders/…), so we EvalSymlinks. A resolve error — e.g. p does not
// exist — falls back to p: a later prefix miss then simply skips, never deletes (fail-safe).
func canonPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}
