package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/panevis"
)

// newHideCmd builds `cc-fleet hide <%N|name@team> [--socket <path>] [--json]` —
// hide a teammate's tmux pane (move it to the detached claude-hidden session)
// without killing the process. Follows teardown.go's --json / SilenceErrors
// discipline.
func newHideCmd() *cobra.Command {
	var asJSON bool
	var socket string
	cmd := &cobra.Command{
		Use:   "hide <%N|name@team>",
		Short: "Hide a teammate's tmux pane without killing it",
		Long: `Move a provider teammate's tmux pane into a detached hidden session so it
disappears from the visible layout — the process keeps running and can be
restored with "cc-fleet show".

The target is a tmux pane id (%42; add --socket <path> when several tmux
servers have that pane) or an agent id (name@team). The origin window is kept
on the pane itself, so show can put it back where it was.

Idempotent: hiding an already-hidden pane returns ok. tmux panes only: a
teammate on a detached swarm server returns SWARM_UNSUPPORTED, one outside
tmux BACKEND_UNSUPPORTED. --json emits the panevis.Result; exit 1 on failure.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportPaneVis(runPaneVis(args[0], socket, true), asJSON)
		},
	}
	addPaneVisFlags(cmd, &asJSON, &socket)
	return cmd
}

func addPaneVisFlags(cmd *cobra.Command, asJSON *bool, socket *string) {
	cmd.Flags().BoolVar(asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")
	cmd.Flags().StringVar(socket, "socket", "",
		"tmux socket path (tmux_socket_path in ps --json) that picks the server of a %N target")
}

// runPaneVis runs Hide (hide=true) or Show on target.
func runPaneVis(target, socket string, hide bool) panevis.Result {
	action := "show"
	if hide {
		action = "hide"
	}
	if onWindows {
		return panevis.Result{Action: action, ErrorCode: panevis.ErrUnsupportedOnWindows, ErrorMsg: windowsUnsupportedMsg(action)}
	}
	if hide {
		return panevis.Hide(target, socket)
	}
	return panevis.Show(target, socket)
}

// reportPaneVis formats a hide/show result. JSON mode emits one object, then
// exits via os.Exit so cobra's error echo can't append a second line that
// breaks JSON parsers.
func reportPaneVis(r panevis.Result, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hide/show: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		if !r.OK {
			os.Exit(1)
		}
		return nil
	}

	if r.OK {
		fmt.Printf("%s %s: ok (hidden=%v)\n", r.Action, r.AgentID, r.Hidden)
		return nil
	}
	fmt.Fprintf(os.Stderr, "%s %s: %s: %s\n", r.Action, r.AgentID, r.ErrorCode, r.ErrorMsg)
	if r.Suggestion != "" {
		fmt.Fprintln(os.Stderr, "  suggestion:", r.Suggestion)
	}
	os.Exit(1)
	return nil
}
