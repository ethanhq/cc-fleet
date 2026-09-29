package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// newTeardownCmd builds `cc-fleet teardown <%N|name@team|team> [--socket <path>] [--json]`.
func newTeardownCmd() *cobra.Command {
	var asJSON bool
	var socket string

	cmd := &cobra.Command{
		Use:   "teardown <%N|name@team|team>",
		Short: "Kill cc-fleet provider teammates (unix-only)",
		Long: `Kill the cc-fleet provider teammates that cc-fleet ps attributes.

The target is a tmux pane id (%42; add --socket <path> when several tmux
servers have that pane), an agent id (name@team), or a team (session-xxxxxxxx
or the team of a listed teammate). Each teammate's identity (pane, exact argv,
process start time) is re-verified right before it is killed; one that no
longer matches is reported as skipped (IDENTITY_MISMATCH) and left alone.
In-process teammates cannot be killed from outside the lead (IN_PROCESS).
Claude Code's team directories are never modified, and the lead and native
teammates are never touched.

Idempotent: a target with nothing left to kill returns ok=true with an empty
killed list.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportTeardown(runTeardown(args[0], socket, diagLogger(cmd)), asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")
	cmd.Flags().StringVar(&socket, "socket", "",
		"tmux socket path (tmux_socket_path in ps --json) that picks the server of a %N target")

	return cmd
}

// runTeardown parses arg and tears the target down.
func runTeardown(arg, socket string, dg *diag.Logger) teardown.Result {
	res := teardown.Result{Target: arg, Killed: []teardown.Killed{}, Skipped: []teardown.Skipped{}}
	if onWindows {
		res.ErrorCode, res.ErrorMsg = teardown.ErrCodeUnsupportedOnWindows, windowsUnsupportedMsg("teardown")
		return res
	}
	t, err := teardown.ParseTarget(arg, socket)
	if err != nil {
		res.ErrorCode, res.ErrorMsg = teardown.ErrCodeBadArgs, err.Error()
		res.Suggestion = "use a pane id (%N, with --socket <path> when several tmux servers have it), an agent id (name@team) or a team name"
		return res
	}
	return teardown.Teardown(t, dg)
}

// reportTeardown formats res for stdout. JSON mode emits exactly one
// envelope and exits via os.Exit so cobra's error echo path doesn't
// append a second line that would break JSON parsers.
func reportTeardown(res teardown.Result, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintln(os.Stderr, "teardown: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		if res.OK {
			return nil
		}
		os.Exit(1)
	}

	if !res.OK {
		fmt.Fprintf(os.Stderr, "teardown: %s: %s\n", res.ErrorCode, res.ErrorMsg)
		if res.Suggestion != "" {
			fmt.Fprintln(os.Stderr, "  suggestion:", res.Suggestion)
		}
		os.Exit(1)
	}
	if len(res.Killed) == 0 && len(res.Skipped) == 0 {
		fmt.Printf("nothing to tear down for %q\n", res.Target)
	}
	for _, k := range res.Killed {
		if k.PaneID != "" {
			fmt.Printf("killed %s (pane %s on %s)\n", k.AgentID, k.PaneID, k.Socket)
		} else {
			fmt.Printf("killed %s (pid %d)\n", k.AgentID, k.PID)
		}
	}
	for _, s := range res.Skipped {
		fmt.Printf("skipped %s: %s\n", s.AgentID, s.Reason)
	}
	if res.Suggestion != "" {
		fmt.Println("  suggestion:", res.Suggestion)
	}
	return nil
}
