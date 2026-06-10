package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// newSpawnCmd builds the `cc-fleet spawn [provider]` command (provider optional →
// stable — the skill drives this surface and reads the --json envelope.
func newSpawnCmd() *cobra.Command {
	var (
		agentName      string
		team           string
		model          string
		color          string
		target         string
		probe          bool
		autoTeam       bool
		leadSessionID  string
		asJSON         bool
		permissionMode string
		dangerSkip     bool
		verify         bool
	)

	cmd := &cobra.Command{
		Use:   "spawn [provider]",
		Short: "Spawn a provider teammate as a tmux pane (Claude layer)",
		Long: `Spawn a provider teammate into a tmux pane using cc-fleet's cached
fingerprint + the provider's profile. Designed to be invoked by the
cc-fleet skill via Bash; the --json flag emits a machine-readable
envelope that the skill switches on.

The team is registered (or created with --auto-team), the agent's inbox
file is pre-created, and the tmux split-window is run with the apiKeyHelper
profile that lazily fetches the provider key.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			provider, perr := resolveProviderArg(firstArg(args))
			if perr != nil {
				return reportSpawn(spawn.Result{OK: false,
					ErrorCode: providerErrorCode(perr), ErrorMsg: perr.Error()}, asJSON)
			}
			if onWindows {
				res := spawn.Result{OK: false, ErrorCode: "UNSUPPORTED_ON_WINDOWS", ErrorMsg: windowsUnsupportedMsg("spawn"), Provider: provider}
				return reportSpawn(res, asJSON)
			}
			// Team / agent names flow into filesystem paths (config.json, inbox
			// files, lock files) and tmux labels. Reject path traversal /
			// separators / absolute paths via the typed constructors BEFORE any
			// spawn state mutation runs.
			if team != "" {
				if _, err := ids.NewTeamID(team); err != nil {
					res := spawn.Result{OK: false, ErrorCode: "BAD_ARGS", ErrorMsg: err.Error(), Provider: provider}
					return reportSpawn(res, asJSON)
				}
			}
			if agentName != "" {
				if _, err := ids.NewAgentName(agentName); err != nil {
					res := spawn.Result{OK: false, ErrorCode: "BAD_ARGS", ErrorMsg: err.Error(), Provider: provider}
					return reportSpawn(res, asJSON)
				}
			}
			// Resolve the manual permission override (if any) and reject
			// contradictory / invalid flags BEFORE any spawn side effect.
			permOverride, permErr := resolvePermissionOverride(permissionMode, dangerSkip)
			if permErr != nil {
				res := spawn.Result{OK: false, ErrorCode: "BAD_ARGS", ErrorMsg: permErr.Error(), Provider: provider}
				return reportSpawn(res, asJSON)
			}
			req := spawn.Request{
				Provider:               provider,
				AgentName:              agentName,
				Team:                   team,
				Model:                  model,
				Color:                  color,
				Target:                 target,
				Probe:                  probe,
				AutoTeam:               autoTeam,
				LeadSessionID:          leadSessionID,
				PermissionModeOverride: permOverride,
				Verify:                 verify,
				Diag:                   diagLogger(cmd),
			}
			res := spawn.Spawn(req)
			return reportSpawn(res, asJSON)
		},
	}

	cmd.Flags().StringVar(&agentName, "as", "",
		"Teammate name (required, e.g. worker-1)")
	cmd.Flags().StringVar(&team, "team", "",
		"Target team (required in Stage 6)")
	cmd.Flags().StringVar(&model, "model", "",
		"Provider model id (default: provider's default_model)")
	cmd.Flags().StringVar(&color, "color", "",
		"Pane color (default: auto-pick from palette)")
	cmd.Flags().StringVar(&target, "target", "",
		"tmux target (session/window/pane; default: pick largest attached session)")
	cmd.Flags().BoolVar(&probe, "probe", true,
		"Probe provider reachability before spawning (3s timeout)")
	// Convenience negate flag so the skill can pass --no-probe without
	// pflag's --probe=false syntax. We mutate the same backing var.
	var noProbe bool
	cmd.Flags().BoolVar(&noProbe, "no-probe", false,
		"Skip the provider reachability probe (overrides --probe)")
	cmd.Flags().BoolVar(&autoTeam, "auto-team", true,
		"Create the team config if it doesn't exist")
	var noAutoTeam bool
	cmd.Flags().BoolVar(&noAutoTeam, "no-auto-team", false,
		"Fail if the team config doesn't exist (overrides --auto-team)")
	cmd.Flags().StringVar(&leadSessionID, "lead-session-id", "",
		"Override the parent session UUID (default: read from team config or generate)")
	cmd.Flags().StringVar(&permissionMode, "permission-mode", "",
		"Force teammate permission mode (default|acceptEdits|plan|auto|bypassPermissions); default: inherit the lead session's startup mode")
	cmd.Flags().BoolVar(&dangerSkip, "dangerously-skip-permissions", false,
		"Force the teammate to skip permission prompts (alias for --permission-mode bypassPermissions)")
	cmd.Flags().BoolVar(&verify, "verify", true,
		"After spawning, confirm the teammate process settled; only runs when the live CC is newer than the bundled recipe")
	var noVerify bool
	cmd.Flags().BoolVar(&noVerify, "no-verify", false,
		"Skip the post-spawn settle check (overrides --verify)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	// Wire the negate flags by adjusting the live values in PreRunE — that
	// gives consistent semantics regardless of flag order on the command line.
	cmd.PreRunE = func(_ *cobra.Command, _ []string) error {
		if noProbe {
			probe = false
		}
		if noAutoTeam {
			autoTeam = false
		}
		if noVerify {
			verify = false
		}
		return nil
	}

	return cmd
}

// resolvePermissionOverride maps the two manual permission flags to a single
// permission-mode value (or "" = none). --dangerously-skip-permissions is sugar
// for --permission-mode bypassPermissions; passing both is rejected (mutually
// exclusive); an out-of-set --permission-mode is rejected before any side effect.
// Callers interpret "" per their model: spawn infers from the lead session, run
// adds no permission flag. Shared by the spawn and run commands.
func resolvePermissionOverride(mode string, danger bool) (string, error) {
	if danger && mode != "" {
		return "", errors.New("--dangerously-skip-permissions and --permission-mode are mutually exclusive")
	}
	if danger {
		return permmode.BypassPermissions, nil
	}
	if mode == "" {
		return "", nil
	}
	if !permmode.IsValid(mode) {
		return "", fmt.Errorf("invalid --permission-mode %q (want one of: %s)", mode, strings.Join(permmode.Modes, ", "))
	}
	return mode, nil
}

// reportSpawn writes the Result. In JSON mode it prints exactly one envelope
// on stdout and calls os.Exit directly (so main()'s err-echo never appends a
// second line that would break JSON consumers). In pretty mode it returns
// an error and lets cobra handle the exit code via SilenceErrors.
func reportSpawn(res spawn.Result, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintln(os.Stderr, "spawn: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		if res.OK {
			return nil
		}
		// Bypass cobra's stderr echo path entirely — JSON consumers expect
		// exactly one envelope.
		os.Exit(1)
	}

	if res.OK {
		fmt.Printf("spawned %s (pane %s, model %s, color %s) in tmux session %s\n",
			res.AgentID, res.PaneID, res.Model, res.Color, res.TmuxSession)
		return nil
	}
	fmt.Fprintf(os.Stderr, "spawn: %s: %s\n", res.ErrorCode, res.ErrorMsg)
	if res.Suggestion != "" {
		fmt.Fprintln(os.Stderr, "suggestion:", res.Suggestion)
	}
	// Same pattern as JSON mode — we've already printed our own message, so
	// suppress cobra's echo by exiting directly.
	os.Exit(1)
	return nil
}
