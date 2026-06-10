// Package profile generates the per-provider JSON files that Claude Code loads
// via its `--settings` flag (a.k.a. profiles).
//
// One file per provider lives at ~/.claude/profiles/<provider>.json with mode 0600.
// The file pins two things — the apiKeyHelper command that fetches the key on
// demand, and the ANTHROPIC_BASE_URL env var that routes traffic to the
// provider's Anthropic-compatible endpoint. Everything else inherits from the
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
// (WriteForProvider) MkdirAll it on demand.
func ProfilesDir() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("profile: HOME is not set")
	}
	return filepath.Join(home, ".claude", "profiles"), nil
}

// ProfilePath returns the absolute path to <ProfilesDir>/<provider>.json.
//
// provider must be a non-empty plain provider name (e.g. "deepseek").
//
// Defense-in-depth: provider is validated against the path/shell-safe grammar AND
// the constructed path is checked to stay under ProfilesDir, so even a malformed
// name that slipped past config Load can't escape ~/.claude/profiles/.
func ProfilePath(provider string) (string, error) {
	if provider == "" {
		return "", errors.New("profile: provider is empty")
	}
	if err := ids.ValidateProviderName(provider); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	dir, err := ProfilesDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, provider+".json")
	if err := ids.EnsureUnderRoot(dir, path); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return path, nil
}
