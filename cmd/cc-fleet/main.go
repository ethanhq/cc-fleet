// Command cc-fleet is the CLI entry point for the cc-fleet vendor-profile manager.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/version"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cc-fleet",
		Short: "Manage Claude Code vendor profiles, secrets, and tmux-spawned teammates",
		Long: `cc-fleet is a tool for managing third-party LLM vendor profiles for Claude Code.

It generates ~/.claude/profiles/<vendor>.json files, dispatches API keys via
pluggable secret backends, captures Claude Code settings fingerprints, and spawns
teammate Claude Code sessions inside tmux windows.`,
		Version:       version.Resolve(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// A bare `cc-fleet` from an interactive terminal launches the TUI;
		// every non-interactive context (pipe, redirect, CI, `</dev/null`)
		// falls through to help so scripts and agents never block. Subcommands
		// bypass this entirely — cobra only calls root Run when none matched.
		Run: func(cmd *cobra.Command, args []string) {
			handled, err := runTUIIfInteractive(args)
			if err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet:", err)
				os.Exit(1)
			}
			if handled {
				return
			}
			_ = cmd.Help()
		},
	}
	root.AddCommand(newKeygetCmd())
	root.AddCommand(newRefreshFingerprintCmd())
	root.AddCommand(newSpawnCmd())
	root.AddCommand(newSubagentCmd())
	root.AddCommand(newSubagentStatusCmd())
	root.AddCommand(newSubagentGCCmd())
	root.AddCommand(newWorkflowCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newCodexCmd())
	root.AddCommand(newCodexProxyCmd())
	root.AddCommand(newTeardownCmd())
	root.AddCommand(newHideCmd())
	root.AddCommand(newShowCmd())
	root.AddCommand(newPsCmd())
	root.AddCommand(newWatchCmd())
	root.AddCommand(newModelsCmd())
	root.AddCommand(newRefreshCmd())
	root.AddCommand(newDoctorCmd())
	// user-layer CRUD.
	root.AddCommand(newInitCmd())
	root.AddCommand(newAddCmd())
	root.AddCommand(newEditCmd())
	root.AddCommand(newRemoveCmd())
	root.AddCommand(newListCmd())
	root.AddCommand(newRepairCmd())
	root.AddCommand(newUninstallCmd())
	return root
}

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cc-fleet:", err)
		os.Exit(1)
	}
}
