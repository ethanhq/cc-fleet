// Package profile generates the per-vendor JSON files that Claude Code loads
// via its `--settings` flag (a.k.a. profiles).
//
// One file per vendor lives at ~/.claude/profiles/<vendor>.json with mode 0600.
// The file pins two things — the apiKeyHelper command that fetches the key on
// demand, and the ANTHROPIC_BASE_URL env var that routes traffic to the
// vendor's Anthropic-compatible endpoint. Everything else inherits from the
// user's ~/.claude/settings.json via Claude Code's normal settings merge.
//
// Profiles intentionally do NOT follow XDG: Claude Code reads ~/.claude/
// unconditionally, so this package only consults $HOME.
package profile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/ids"
)

// ProfilesDir returns the absolute path to ~/.claude/profiles/.
//
// It errors if $HOME is unset; the directory is not created here — writers
// (WriteForVendor) MkdirAll it on demand.
func ProfilesDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("profile: HOME is not set")
	}
	return filepath.Join(home, ".claude", "profiles"), nil
}

// ProfilePath returns the absolute path to <ProfilesDir>/<vendor>.json.
//
// vendor must be a non-empty plain vendor name (e.g. "deepseek").
//
// Defense-in-depth: vendor is validated against the path/shell-safe grammar AND
// the constructed path is checked to stay under ProfilesDir, so even a malformed
// name that slipped past config Load can't escape ~/.claude/profiles/.
func ProfilePath(vendor string) (string, error) {
	if vendor == "" {
		return "", errors.New("profile: vendor is empty")
	}
	if err := ids.ValidateVendorName(vendor); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	dir, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, vendor+".json")
	if err := ids.EnsureUnderRoot(dir, path); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return path, nil
}
