package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/teammate"
)

// Removed-command copy: error_msg says what happened,
// suggestion says what to do next.
const (
	spawnRemovedMsg        = "`cc-fleet spawn` was removed: Claude Code now starts teammates natively"
	spawnRemovedSuggestion = "run `cc-fleet teammate check <provider> --json`, then `Agent({name, subagent_type:<agent_type>, prompt})`"
)

// newSpawnCmd is the hidden stub left where 0.3.x's `cc-fleet spawn` was:
// Claude Code now starts provider teammates natively through the launcher shim.
func newSpawnCmd() *cobra.Command {
	return newRemovedCmd("spawn", spawnRemovedMsg, spawnRemovedSuggestion)
}

// removedEnvelope is the only output of a removed command.
type removedEnvelope struct {
	OK         bool   `json:"ok"`
	ErrorCode  string `json:"error_code"`
	ErrorMsg   string `json:"error_msg"`
	Suggestion string `json:"suggestion"`
}

// newRemovedCmd builds a hidden stub for a removed command. Flag parsing is off,
// so it accepts any arguments (even --help): it prints one COMMAND_REMOVED
// envelope on stdout — JSON with or without --json, since only the skill calls
// these — and exits 1.
func newRemovedCmd(use, msg, suggestion string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              "Removed command (prints COMMAND_REMOVED)",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false) // keep <provider> readable for the model reading stdout
			if err := enc.Encode(removedEnvelope{
				ErrorCode:  teammate.CodeCommandRemoved,
				ErrorMsg:   msg,
				Suggestion: suggestion,
			}); err != nil {
				fmt.Fprintln(os.Stderr, use+": encode:", err)
			}
			// Exit directly so main() does not append a second (stderr) line.
			os.Exit(1)
			return nil
		},
	}
}
