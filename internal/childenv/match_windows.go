//go:build windows

package childenv

import "strings"

// dropListUpper is dropList keyed by upper-cased name. Windows env names are
// case-insensitive, so a child that sets `anthropic_api_key=…` addresses the
// same variable; folding case catches it. Safe by the dropList case-fold
// invariant — every entry is an ANTHROPIC_*/CLAUDE* name.
var dropListUpper = upperKeys(dropList)

// inDropList reports whether name is a scrubbed variable, matching case-
// insensitively to mirror Windows env-name semantics.
func inDropList(name string) bool {
	return dropListUpper[strings.ToUpper(name)]
}

// teammateScrubUpper is teammateScrub keyed by upper-cased name.
var teammateScrubUpper = upperKeys(teammateScrub)

// inTeammateScrub reports whether CleanForTeammate drops name, matching case-
// insensitively like inDropList.
func inTeammateScrub(name string) bool {
	u := strings.ToUpper(name)
	return teammateScrubUpper[u] || hasTeammateScrubPrefix(u)
}
