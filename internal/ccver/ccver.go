// Package ccver locates the installed `claude` binary and reports its version.
//
// The doctor's CheckClaudeBinary and CheckFingerprint use this to confirm a
// working CC install is reachable and to compare the cached fingerprint's
// cc_version against the binary that's actually on disk.
//
// Resolution order matches what the user's PATH-driven workflow expects, then
// falls back to the per-version layout:
//  1. exec.LookPath("claude")
//  2. ~/.local/share/claude/versions/<semver>/  — picks the largest semver
//
// Version detection prefers the binary path's basename (cheap, no exec); if
// that fails the binary is asked directly with a bounded `--version` call.
package ccver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
)

// versionRegex extracts a semver-ish "MAJOR.MINOR.PATCH" out of a basename or
// a `claude --version` output token. Matches at end-of-string so it pairs
// cleanly with path basenames like ".../2.1.150".
var versionRegex = regexp.MustCompile(`(\d+\.\d+\.\d+)$`)

// versionExecTimeout caps the fallback `claude --version` invocation so a
// wedged binary can't hang doctor.
const versionExecTimeout = 5 * time.Second

// ErrNotFound is returned by Detect when no `claude` binary can be located on
// PATH or in the per-version layout.
var ErrNotFound = errors.New("claude binary not found")

// Detect locates the installed `claude` binary and returns its absolute path
// plus a best-effort version string. An empty version is reported (no error)
// when the path is found but version detection fails; callers can surface that
// as a "version unknown" warning rather than a hard failure.
func Detect() (binaryPath, version string, err error) {
	binaryPath, err = locate()
	if err != nil {
		return "", "", err
	}
	version = VersionForPath(binaryPath)
	return binaryPath, version, nil
}

// locate returns the absolute path of the `claude` binary or ErrNotFound. The
// PATH lookup runs first because that's what the user's shell does; the
// per-version layout fallback covers the case where claude was installed via
// the official installer but the bin shim isn't (yet) on PATH.
func locate() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		// LookPath may return a relative result if PATH contains relative
		// dirs; absolutize for consistent reporting.
		if abs, absErr := filepath.Abs(p); absErr == nil {
			return abs, nil
		}
		return p, nil
	}

	home := homeForLayout()
	if home == "" {
		return "", ErrNotFound
	}
	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	entries, err := os.ReadDir(versionsDir)
	if err != nil {
		return "", ErrNotFound
	}

	// Collect every entry whose name parses as semver, sort descending, then
	// pick the FIRST one that actually holds an executable claude (claudeBinName:
	// `claude` on unix, `claude.exe` on windows).
	//
	// Descend candidates by descending semver and commit only to a dir whose
	// <dir>/claudeBinName is an executable regular file: a versioned dir with no
	// runnable claude must NOT be reported as the install (that's a false-healthy
	// doctor and a spawn that selects a nonexistent binary).
	type sv struct {
		name string
		nums [3]int
	}
	var cands []sv
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nums, ok := parseSemver(e.Name())
		if !ok {
			continue
		}
		cands = append(cands, sv{name: e.Name(), nums: nums})
	}
	if len(cands) == 0 {
		return "", ErrNotFound
	}
	sort.Slice(cands, func(i, j int) bool {
		for k := 0; k < 3; k++ {
			if cands[i].nums[k] != cands[j].nums[k] {
				return cands[i].nums[k] > cands[j].nums[k]
			}
		}
		return false
	})
	for _, c := range cands {
		cand := filepath.Join(versionsDir, c.name, claudeBinName)
		if isExecutableFile(cand) {
			return cand, nil
		}
	}
	// Every versioned dir was empty / lacked an executable claude — same outcome
	// as no versioned install at all, so the caller (doctor) Fails rather than
	// reporting a phantom binary it can never run.
	return "", ErrNotFound
}

// parseSemver returns the three components of a "X.Y.Z" name. Returns ok=false
// for anything that doesn't have exactly three dotted numeric parts.
func parseSemver(name string) (out [3]int, ok bool) {
	parts := strings.Split(name, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// VersionForPath returns a best-effort version for the binary AT path: the
// per-version layout basename first (cheap, no exec), else a bounded
// `<path> --version`. Unlike Detect it never re-locates — the caller has
// already chosen the exact executable, so its version is the one reported.
// "" means "version unknown" (path not in the layout and exec failed).
func VersionForPath(path string) string {
	if v := versionFromPath(path); v != "" {
		return v
	}
	return versionFromExec(path)
}

// AtLeast reports whether version is a parseable semver >= floor. An empty or
// unparseable version returns false, letting a caller treat "unknown" as
// below-floor (fail open to the conservative path). floor is assumed valid.
func AtLeast(version, floor string) bool {
	v, ok := parseSemver(version)
	if !ok {
		return false
	}
	f, ok := parseSemver(floor)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if v[i] != f[i] {
			return v[i] > f[i]
		}
	}
	return true
}

// versionFromPath pulls a "X.Y.Z" out of the directory containing the binary
// (the per-version install layout). It also handles the case where the binary
// path itself ends with the version (less common but cheap to support).
func versionFromPath(binaryPath string) string {
	if binaryPath == "" {
		return ""
	}
	// First try the immediate parent: .../versions/2.1.150/claude
	if m := versionRegex.FindStringSubmatch(filepath.Base(filepath.Dir(binaryPath))); len(m) >= 2 {
		return m[1]
	}
	// Fallback: basename of the binary itself (e.g. /opt/claude-2.1.150)
	if m := versionRegex.FindStringSubmatch(filepath.Base(binaryPath)); len(m) >= 2 {
		return m[1]
	}
	return ""
}

// versionFromExec invokes `<binary> --version` with a bounded timeout. Returns
// the first whitespace-separated token that matches versionRegex, or "" on any
// failure (timeout, non-zero exit, no match). Empty return is treated by
// callers as "version unknown" rather than "no binary".
func versionFromExec(binaryPath string) string {
	if binaryPath == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	cmd.Env = childenv.Clean(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, field := range strings.Fields(string(out)) {
		if m := versionRegex.FindStringSubmatch(field); len(m) >= 2 {
			return m[1]
		}
	}
	return ""
}

// String renders a (path, version) pair into a single human line for use in
// doctor's CheckResult.Detail. Empty version is rendered as "(version unknown)".
func String(binaryPath, version string) string {
	if version == "" {
		return fmt.Sprintf("%s (version unknown)", binaryPath)
	}
	return fmt.Sprintf("%s (cc %s)", binaryPath, version)
}
