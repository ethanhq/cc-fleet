package main

import "github.com/spf13/cobra"

// Removed-command copy.
const (
	refreshFingerprintRemovedMsg        = "fingerprints were removed: Claude Code builds the teammate command itself"
	refreshFingerprintRemovedSuggestion = "nothing to refresh; run `cc-fleet doctor` if something fails"
)

// newRefreshFingerprintCmd is the hidden stub left where 0.3.x's
// `cc-fleet refresh-fingerprint` was (see newRemovedCmd in spawn.go).
func newRefreshFingerprintCmd() *cobra.Command {
	return newRemovedCmd("refresh-fingerprint", refreshFingerprintRemovedMsg, refreshFingerprintRemovedSuggestion)
}
