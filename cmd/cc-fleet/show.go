package main

import (
	"github.com/spf13/cobra"
)

// newShowCmd builds `cc-fleet show <target> [--json]` — restore a previously
// hidden teammate pane back into its origin window. Shares runPaneVis /
// reportPaneVis with the hide command.
func newShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <team|team/member|name@team|%pane>",
		Short: "Restore a hidden teammate's tmux pane",
		Long: `Join a previously hidden teammate's pane back into the window it was
hidden from, reflow the layout to main-vertical, and pin the leader to 30%.

The target may be a tmux pane id (%42), a "team/member", a "name@team" agent id,
or a bare team name (shows every member of the team that has a pane). Showing a
member that isn't hidden returns NOT_HIDDEN; one whose origin window wasn't
recorded returns NO_ORIGIN; an out-of-tmux swarm teammate returns
SWARM_UNSUPPORTED (hide/show is in-tmux only).

--json emits the panevis.Result (single object, or an array when the target
expands to several members); exit 1 if any member failed.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPaneVis(args[0], asJSON, false)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")
	return cmd
}
