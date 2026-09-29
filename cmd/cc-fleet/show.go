package main

import (
	"github.com/spf13/cobra"
)

// newShowCmd builds `cc-fleet show <%N|name@team> [--socket <path>] [--json]` —
// restore a previously hidden teammate pane back into its origin window.
// Shares runPaneVis / reportPaneVis with the hide command.
func newShowCmd() *cobra.Command {
	var asJSON bool
	var socket string
	cmd := &cobra.Command{
		Use:   "show <%N|name@team>",
		Short: "Restore a hidden teammate's tmux pane",
		Long: `Join a previously hidden teammate's pane back into the window it was
hidden from, reflow the layout to main-vertical, and pin the leader to 30%.

The target is a tmux pane id (%42; add --socket <path> when several tmux
servers have that pane) or an agent id (name@team). Showing a teammate that
isn't hidden returns NOT_HIDDEN; one whose origin window wasn't recorded
returns NO_ORIGIN; a teammate on a detached swarm server returns
SWARM_UNSUPPORTED.

--json emits the panevis.Result; exit 1 on failure.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportPaneVis(runPaneVis(args[0], socket, false), asJSON)
		},
	}
	addPaneVisFlags(cmd, &asJSON, &socket)
	return cmd
}
