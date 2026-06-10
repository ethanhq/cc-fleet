// Command cc-fleet is the CLI entry point for the cc-fleet provider-profile manager.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// verboseFlag backs the root --verbose persistent flag. Commands read it at
// RunE time through diagLogger; it is never consulted before Execute parses.
var verboseFlag bool

// diagLogger returns the command's --verbose diagnostic sink: stderr when the
// flag is set, else nil (a nil *diag.Logger is a no-op everywhere downstream).
func diagLogger(cmd *cobra.Command) *diag.Logger {
	if !verboseFlag {
		return nil
	}
	return diag.New(cmd.ErrOrStderr())
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cc-fleet",
		Short: "Manage Claude Code provider profiles, secrets, and tmux-spawned teammates",
		Long: `cc-fleet is a tool for managing third-party LLM provider profiles for Claude Code.

It generates ~/.claude/profiles/<provider>.json files, dispatches API keys via
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
			handled, err := runTUIIfInteractive(args, verboseFlag)
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
	root.PersistentFlags().BoolVar(&verboseFlag, "verbose", false,
		"step-trace diagnostics: stderr for commands, a 0600 log file for the TUI")
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
	root.AddCommand(newDefaultCmd())
	root.AddCommand(newRepairCmd())
	root.AddCommand(newUninstallCmd())
	root.AddCommand(newUpdateCmd())
	return root
}

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cc-fleet:", err)
		os.Exit(1)
	}
}
