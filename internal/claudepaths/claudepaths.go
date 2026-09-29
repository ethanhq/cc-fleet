// Package claudepaths resolves Claude Code's own configuration paths. Every
// path honors CLAUDE_CONFIG_DIR the way Claude Code does, so cc-fleet reads the
// same sessions/, teams/, agents/ and settings.json the running lead uses.
//
// The functions are pure: they read the environment on every call and cache
// nothing, so tests can change HOME / CLAUDE_CONFIG_DIR at any time.
//
// cc-fleet's provider profiles are NOT resolved here: they stay under
// profile.ProfilesDir() (~/.claude/profiles) regardless of CLAUDE_CONFIG_DIR,
// because they are passed to claude as absolute --settings paths.
package claudepaths

import (
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/homedir"
)

// envConfigDir is the variable Claude Code reads to relocate its config root.
const envConfigDir = "CLAUDE_CONFIG_DIR"

// Root returns Claude Code's config root: $CLAUDE_CONFIG_DIR verbatim when it
// is non-empty (no ~ expansion), otherwise <home>/.claude. It returns "" when
// the home directory cannot be resolved; every other function then returns ""
// too, and callers treat that as "not found".
func Root() string {
	if dir := os.Getenv(envConfigDir); dir != "" {
		return dir
	}
	home, err := homedir.Home()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// Settings returns Root()/settings.json (the user-layer settings file).
func Settings() string { return under("settings.json") }

// Agents returns Root()/agents (user-level agent definitions).
func Agents() string { return under("agents") }

// Sessions returns Root()/sessions (one <pid>.json per live Claude Code session).
func Sessions() string { return under("sessions") }

// Teams returns Root()/teams (one directory per agent team).
func Teams() string { return under("teams") }

// GlobalConfig returns Claude Code's global config file: $CLAUDE_CONFIG_DIR/.claude.json
// when CLAUDE_CONFIG_DIR is set, otherwise <home>/.claude.json (a sibling of the
// .claude directory, not inside it). "" when the home directory cannot be resolved.
func GlobalConfig() string {
	if dir := os.Getenv(envConfigDir); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	home, err := homedir.Home()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude.json")
}

// under joins name onto Root(), keeping "" when Root() cannot be resolved.
func under(name string) string {
	root := Root()
	if root == "" {
		return ""
	}
	return filepath.Join(root, name)
}
