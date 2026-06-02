// Package ids validates cc-fleet identifiers (team names, member names) that
// flow into filesystem paths, tmux labels, inbox file names, and agent IDs.
// It is the centralized path-safety boundary: every CLI entry point and every
// helper that builds a team/member path or lock file calls a validator here,
// rejecting separators / ".." / absolute paths before they reach the filesystem.
package ids

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrInvalidTeamName is returned by ValidateTeamName for any input that fails
// the path-safety rules. Use errors.Is for dispatch.
var ErrInvalidTeamName = errors.New("invalid team name")

// ErrInvalidMemberName is the member-name analogue of ErrInvalidTeamName.
var ErrInvalidMemberName = errors.New("invalid member name")

// ErrInvalidJobID is returned by ValidateJobID for a subagent job id that fails
// the path-safety rules. Use errors.Is for dispatch.
var ErrInvalidJobID = errors.New("invalid job id")

// maxIDLen caps identifier length: a defense-in-depth guard against
// pathologically long argv tokens / paths, not a tmux/fs limit.
const maxIDLen = 128

// ValidateTeamName reports an error iff s is unsafe to use as a team identifier
// (path component, lock file segment, etc.). The rule set is the same as
// ValidateMemberName — the only difference is the wrapped sentinel error.
//
// Accepted iff:
//   - non-empty;
//   - not "." or "..";
//   - no '/' or '\\' anywhere in the string;
//   - not an absolute path (no leading separator);
//   - filepath.Clean(s) == s (rejects "x/.." trickery that survives a naive
//     "no .." substring filter);
//   - length <= maxIDLen bytes.
//
// The check is byte-oriented and OS-agnostic: we reject the forward slash AND
// the backslash on every platform so a future Windows port can't reintroduce
// the same traversal hole through path conventions.
func ValidateTeamName(s string) error {
	if err := validateID(s); err != nil {
		return fmt.Errorf("%w %q: %s", ErrInvalidTeamName, s, err.Error())
	}
	return nil
}

// ValidateMemberName is the same rule set as ValidateTeamName, used for
// teammate names (which flow into `--agent-id <name>@<team>`, inbox file
// basenames, and panevis/teardown lookups). Returns ErrInvalidMemberName on
// failure.
func ValidateMemberName(s string) error {
	if err := validateID(s); err != nil {
		return fmt.Errorf("%w %q: %s", ErrInvalidMemberName, s, err.Error())
	}
	return nil
}

// ValidateJobID is the same rule set as ValidateTeamName, used for subagent job
// ids (always a uuid.NewString() in practice) that flow into the jobs-dir path
// via filepath.Join. It guards the subagent-status entry point against reading
// outside the jobs directory. Returns ErrInvalidJobID on failure.
func ValidateJobID(s string) error {
	if err := validateID(s); err != nil {
		return fmt.Errorf("%w %q: %s", ErrInvalidJobID, s, err.Error())
	}
	return nil
}

// validateID is the shared rule body. Returns a plain error describing the
// first failing rule; callers wrap it with the right sentinel.
func validateID(s string) error {
	if s == "" {
		return errors.New("empty")
	}
	if len(s) > maxIDLen {
		return fmt.Errorf("longer than %d bytes", maxIDLen)
	}
	if s == "." || s == ".." {
		return errors.New("must not be '.' or '..'")
	}
	// Reject a leading '%': teardown/hide/show treat a '%'-prefixed target as a
	// tmux pane id, so a team named "%prod" would be creatable yet untargetable.
	if strings.HasPrefix(s, "%") {
		return errors.New("must not start with '%' (collides with tmux pane id syntax)")
	}
	if strings.ContainsAny(s, "/\\") {
		return errors.New("must not contain path separators")
	}
	// Reject whitespace: macOS recovers argv via space-joined `ps -o command=`,
	// so a whitespace-bearing name would split into multiple tokens and the
	// --agent-id reap/teardown/ps match would miss it, leaking the vendor process.
	if strings.ContainsAny(s, " \t\r\n\v\f") {
		return errors.New("must not contain whitespace")
	}
	if filepath.IsAbs(s) {
		return errors.New("must not be absolute")
	}
	if filepath.Clean(s) != s {
		// Defense-in-depth against forms like "./x" or " . " that clean to a
		// different value. The separator and "." checks above already cover the
		// known cases; this catches anything we missed.
		return errors.New("must equal filepath.Clean(name)")
	}
	// NUL byte is illegal in filesystem paths on every supported OS; reject
	// belt-and-braces in case the upstream caller built the string from a
	// network source.
	if strings.ContainsRune(s, 0) {
		return errors.New("must not contain NUL")
	}
	return nil
}

// EnsureUnderRoot is the low-level under-root check used by helpers that have
// already built a candidate path: after filepath.Clean(candidate), it must have
// root + separator as a strict prefix. This stops symlink/absolute-path tricks
// that survive the name-level validators (the validator rejects "/x"; this
// rejects a constructed "/etc/passwd" produced via a symlinked root).
//
// root is taken verbatim — callers are responsible for passing the canonical
// $HOME/.claude/teams (or similar) themselves. A trailing separator on root is
// tolerated.
func EnsureUnderRoot(root, candidate string) error {
	if root == "" {
		return errors.New("ids: empty root")
	}
	clean := filepath.Clean(candidate)
	prefix := strings.TrimRight(root, string(filepath.Separator)) + string(filepath.Separator)
	if !strings.HasPrefix(clean, prefix) && clean != strings.TrimRight(root, string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes root %q", clean, root)
	}
	return nil
}
