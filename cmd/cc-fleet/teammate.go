package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/teammate"
)

// newTeammateCmd builds `cc-fleet teammate`: check and
// setup, plus the hidden guard run by the plugin's PreToolUse(Agent) hook.
// `cc-fleet __teammate-launch` is dispatched in main() before cobra.
func newTeammateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "teammate",
		Short: "Provider teammates on Claude Code's native agent teams (check, setup)",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newTeammateCheckCmd(), newTeammateSetupCmd(), newTeammateGuardCmd())
	return cmd
}

// newTeammateCheckCmd: cc-fleet teammate check [<provider>] [--slot default|strong|fast] [--no-probe] [--json]
func newTeammateCheckCmd() *cobra.Command {
	var (
		slot    string
		noProbe bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:           "check [provider]",
		Short:         "Check that a provider teammate can start from this Claude Code session",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			res := teammate.Evaluate(cmd.Context(), teammate.Input{
				Provider: firstArg(args),
				Slot:     teammate.Slot(slot),
				Prepare:  true,
				Probe:    !noProbe,
			})
			if asJSON {
				writeTeammateJSON(cmd.OutOrStdout(), res)
			} else if res.OK {
				fmt.Fprintf(cmd.OutOrStdout(), "teammate check: ok: %s (model %s) in team %s, teammateMode %s (%s)\n",
					res.AgentType, res.Model, res.Team, res.TeammateMode, res.TeammateModeSource)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "teammate check: "+
					teammateFailureText(res.ErrorCode, res.Detail, res.ErrorMsg, res.Suggestion))
			}
			if !res.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&slot, "slot", string(teammate.SlotDefault), "Model slot: default|strong|fast")
	cmd.Flags().BoolVar(&noProbe, "no-probe", false, "Skip the provider reachability probe")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope (for skill consumption)")
	return cmd
}

// newTeammateSetupCmd: cc-fleet teammate setup [--teammate-mode tmux|keep] [--force] [--remove] [--yes] [--json]
func newTeammateSetupCmd() *cobra.Command {
	var (
		mode   string
		force  bool
		remove bool
		yes    bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:           "setup",
		Short:         "Enable provider teammates: launcher shim, agent definitions and Claude Code settings",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := teammate.SetupOptions{TeammateMode: mode, Force: force, Remove: remove}
			var res teammate.SetupResult
			switch {
			case mode != "tmux" && mode != "keep":
				res = teammateSetupRefusal(opts, teammate.CodeBadArgs,
					fmt.Sprintf("--teammate-mode %q is not tmux or keep", mode),
					"pass --teammate-mode tmux or --teammate-mode keep")
			case !yes:
				// Consent is an explicit flag only; never prompt, TTY or not.
				plan := teammateSetupPlan(opts)
				msg := "nothing changed: setup needs explicit consent"
				if asJSON {
					msg += "; it would change " + strings.Join(plan, "; ")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "teammate "+teammateSetupAction(remove)+" would change:")
					for _, l := range plan {
						fmt.Fprintln(cmd.OutOrStdout(), "  "+l)
					}
				}
				res = teammateSetupRefusal(opts, teammate.CodeBadArgs, msg, "rerun with --yes")
			default:
				res = teammate.Setup(opts)
			}
			if asJSON {
				writeTeammateJSON(cmd.OutOrStdout(), res)
			} else if res.OK {
				printTeammateSetup(cmd.OutOrStdout(), res, mode)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "teammate "+res.Action+": "+
					teammateFailureText(res.ErrorCode, res.Detail, res.ErrorMsg, res.Suggestion))
			}
			if !res.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&mode, "teammate-mode", "keep",
		"tmux: set teammateMode to tmux when it is unset or in-process; keep: leave teammateMode alone")
	cmd.Flags().BoolVar(&force, "force", false,
		"Replace a CLAUDE_CODE_TEAMMATE_COMMAND that is not cc-fleet's launcher")
	cmd.Flags().BoolVar(&remove, "remove", false,
		"Remove the launcher setting and cc-fleet's agent definitions (the shim file is kept)")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"Apply the changes (without it, setup only lists what it would change)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

func teammateSetupAction(remove bool) string {
	if remove {
		return "remove"
	}
	return "setup"
}

// teammateSetupRefusal is the SetupResult for a setup the CLI refuses to run.
func teammateSetupRefusal(opts teammate.SetupOptions, code, msg, suggestion string) teammate.SetupResult {
	res := teammate.SetupResult{
		Action:       teammateSetupAction(opts.Remove),
		SettingsPath: claudepaths.Settings(),
		Changed:      []string{},
		Warnings:     []string{},
		ErrorCode:    code,
		ErrorMsg:     msg,
		Suggestion:   suggestion,
	}
	res.Shim, _ = teammate.ShimPath()
	return res
}

// teammateSetupPlan lists the files and keys `teammate setup` (or --remove)
// may change, for the no-consent refusal.
func teammateSetupPlan(opts teammate.SetupOptions) []string {
	shim, _ := teammate.ShimPath()
	state, _ := onboarding.StatePath()
	defs := filepath.Join(claudepaths.Agents(), teammate.TypePrefix+"*.md")
	settings := claudepaths.Settings()
	if opts.Remove {
		return []string{
			settings + ": delete env." + teammate.EnvTeammateCommand + " (only when it names cc-fleet's shim)",
			defs + ": delete the definitions cc-fleet manages",
			state + ": mark the teammate lane disabled",
		}
	}
	keys := "env." + teammate.EnvTeammateCommand + ", env." + teammate.EnvAgentTeams
	if opts.TeammateMode == "tmux" {
		keys += ", " + teammate.KeyTeammateMode + " (only when unset or in-process)"
	}
	return []string{
		shim + ": write the launcher shim",
		defs + ": write one definition per enabled provider",
		settings + ": set " + keys,
		state + ": mark the teammate lane enabled",
	}
}

// printTeammateSetup is the human summary of a successful setup or remove.
func printTeammateSetup(w io.Writer, res teammate.SetupResult, mode string) {
	changed := "nothing changed"
	if len(res.Changed) > 0 {
		changed = strings.Join(res.Changed, ", ")
	}
	if res.Action == "remove" {
		fmt.Fprintf(w, "teammate remove: provider teammates disabled (%s); the shim %s is kept\n", changed, res.Shim)
		return
	}
	fmt.Fprintf(w, "teammate setup: provider teammates enabled (%s)\n", changed)
	switch {
	case res.TeammateMode == "in-process":
		fmt.Fprintf(w, "  note: teammateMode is in-process (%s), so provider teammates are refused; "+
			"rerun with --teammate-mode tmux, or start claude --teammate-mode tmux\n", res.TeammateModeSource)
	case mode == "tmux" && !res.ModeWritten && res.TeammateMode != "tmux":
		fmt.Fprintf(w, "  note: teammateMode stays %s (your choice, left unchanged)\n", res.TeammateMode)
	}
	if res.RestartRequired {
		fmt.Fprintln(w, "  next: restart claude inside tmux to use them")
	}
}

// newTeammateGuardCmd: cc-fleet teammate guard --protocol N (hidden). stdin is
// the hook's JSON; the exit code is Guard's (no envelope).
func newTeammateGuardCmd() *cobra.Command {
	var protocol int
	cmd := &cobra.Command{
		Use:           "guard",
		Short:         "PreToolUse(Agent) verdict for the cc-fleet plugin hook",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			os.Exit(teammate.Guard(cmd.Context(), protocol, cmd.InOrStdin(), cmd.OutOrStdout()))
			return nil
		},
	}
	cmd.Flags().IntVar(&protocol, "protocol", 0, "Guard protocol version the hook script speaks")
	return cmd
}

// writeTeammateJSON prints v as one JSON line without HTML escaping, so the
// copy stays readable for the model reading stdout.
func writeTeammateJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "cc-fleet: marshal:", err)
		os.Exit(1)
	}
}

// teammateFailureText renders "CODE(detail): msg — suggestion", leaving out the
// detail and suggestion parts when they are empty.
func teammateFailureText(code, detail, msg, suggestion string) string {
	s := code
	if detail != "" {
		s += "(" + detail + ")"
	}
	s += ": " + msg
	if suggestion != "" {
		s += " — " + suggestion
	}
	return s
}
