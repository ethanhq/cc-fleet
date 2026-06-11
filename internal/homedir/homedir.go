// Package homedir is the single home-directory resolver for every package that
// roots config state under the user's home (~/.config/cc-fleet, ~/.claude).
// os.UserHomeDir reads $HOME on unix — no passwd consultation in modern Go, so
// tests that t.Setenv("HOME", tempDir) stay hermetic — and %USERPROFILE% on
// Windows, where HOME is not a native variable. Packages share this wrapper so
// the per-platform contract is stated once.
package homedir

import "os"

// Home returns the user's home directory, or an error when the platform's
// home variable is unset.
func Home() (string, error) {
	return os.UserHomeDir()
}
