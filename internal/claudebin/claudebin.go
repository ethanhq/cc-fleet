// Package claudebin resolves the `claude` binary cc-fleet execs. It replaces the
// fingerprint cache: nothing is read from fingerprint.json, so a CC upgrade can
// never leave cc-fleet running a stale recorded binary.
//
// Resolve is the live lookup (ccver: PATH, then the per-version layout) checked
// by ccver.IsExecutable; ForSession prefers the exact version a Claude Code
// session reports. Executability has one implementation, in ccver, per platform.
package claudebin

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/homedir"
)

// ErrNotFound is returned (wrapped) when no runnable claude binary exists.
var ErrNotFound = errors.New("claude binary not found")

// versions caches path → version for the process lifetime, so repeated
// resolutions never re-run `claude --version`.
var versions sync.Map

// Resolve returns the live claude binary and its version. The binary is located
// the way ccver.Detect does (PATH first, then the per-version layout) and must
// pass ccver.IsExecutable. The version is ccver.VersionForPath of that path,
// computed once per path per process; "" means unknown and is not an error.
// Every failure satisfies errors.Is(err, ErrNotFound).
func Resolve() (path, version string, err error) {
	path, ok := ccver.Located()
	if !ok {
		return "", "", fmt.Errorf("%w: not on PATH or under ~/.local/share/claude/versions", ErrNotFound)
	}
	if !ccver.IsExecutable(path) {
		return "", "", fmt.Errorf("%w: %s is not a runnable file", ErrNotFound, path)
	}
	if v, ok := versions.Load(path); ok {
		return path, v.(string), nil
	}
	version = ccver.VersionForPath(path)
	versions.Store(path, version)
	return path, version, nil
}

// ForSession returns <home>/.local/share/claude/versions/<version> when it passes
// ccver.IsExecutable — the exact binary a Claude Code session of that version
// runs. An empty or path-unsafe version, or a file that does not qualify, falls
// back to Resolve's path.
func ForSession(version string) (string, error) {
	if version != "" && version != "." && version != ".." && filepath.Base(version) == version {
		if home, err := homedir.Home(); err == nil && home != "" {
			p := filepath.Join(home, ".local", "share", "claude", "versions", version)
			if ccver.IsExecutable(p) {
				return p, nil
			}
		}
	}
	path, _, err := Resolve()
	return path, err
}
