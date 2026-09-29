package teammate

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// Claude Code's own path-name rules, mirrored so cc-fleet reads the files CC
// writes. Both are JS regex replaces over UTF-16 code units, so a character
// outside the BMP (a surrogate pair) becomes two dashes.

// SanitizeTeamDir mirrors Claude Code's team-directory rule (mQt):
// every non-alphanumeric becomes '-', then the result is lower-cased.
func SanitizeTeamDir(name string) string {
	return strings.ToLower(ccReplace(name, func(r rune) bool { return isASCIIAlnum(r) }))
}

// SanitizeInboxName mirrors Claude Code's inbox file-name rule (aP):
// every character outside [A-Za-z0-9_-] becomes '-'; case is kept.
func SanitizeInboxName(name string) string {
	return ccReplace(name, func(r rune) bool { return isASCIIAlnum(r) || r == '_' || r == '-' })
}

// InboxPath returns Teams()/<SanitizeInboxName(team)>/inboxes/<SanitizeInboxName(name)>.json,
// checked to stay under Teams(). Claude Code's inbox path function applies aP to
// the team name too (not mQt), so the team segment keeps case and '_'.
// It only builds the path; nothing is read or written.
func InboxPath(team, name string) (string, error) {
	if team == "" || name == "" {
		return "", errors.New("teammate: inbox path needs a team and a name")
	}
	root := claudepaths.Teams()
	if root == "" {
		return "", errors.New("teammate: cannot resolve the Claude Code teams directory")
	}
	out := filepath.Join(root, SanitizeInboxName(team), "inboxes", SanitizeInboxName(name)+".json")
	if err := ids.EnsureUnderRoot(root, out); err != nil {
		return "", fmt.Errorf("teammate: %w", err)
	}
	return out, nil
}

func isASCIIAlnum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// ccReplace replaces every rune that keep rejects with '-', once per UTF-16
// code unit (twice for a rune above U+FFFF), matching a JS /[^…]/g replace.
func ccReplace(s string, keep func(rune) bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case keep(r):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
