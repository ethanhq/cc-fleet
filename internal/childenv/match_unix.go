//go:build !windows

package childenv

// inDropList reports whether name is a scrubbed variable. Unix env names are
// case-sensitive, so an exact-case lookup of the canonical dropList suffices.
func inDropList(name string) bool {
	return dropList[name]
}

// inTeammateScrub reports whether CleanForTeammate drops name (exact case).
func inTeammateScrub(name string) bool {
	return teammateScrub[name] || hasTeammateScrubPrefix(name)
}
