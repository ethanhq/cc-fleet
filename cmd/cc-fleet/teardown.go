package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// newTeardownCmd builds `cc-fleet teardown <team-or-pane> [--json]`.
//
// The argument is interpreted as a tmux pane id when it starts with "%"
// (matching tmux's own pane-id format), otherwise as a team name. We do
// not require an explicit flag because the two namespaces are disjoint —
// team names can't start with %.
func newTeardownCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "teardown <team-or-pane>",
		Short: "Kill teammate panes and clean up team state",
		Long: `Clean up cc-fleet teammates and their state.

The argument is treated as a tmux pane id when it starts with "%"
(e.g. %42), otherwise as a team name. Pane teardown kills only that pane
and detaches its member entry from the owning team. Team teardown kills
every registered pane and removes ~/.claude/teams/<team>/ entirely.

Idempotent: tearing down a non-existent team or pane returns ok=true so
skill flows don't fail on retries.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if onWindows {
				res := teardown.Result{OK: false, Target: args[0], ErrorCode: teardown.ErrCodeInternal, ErrorMsg: windowsUnsupportedMsg("teardown")}
				return reportTeardown(res, asJSON)
			}
			target := args[0]
			var res teardown.Result
			if strings.HasPrefix(target, "%") {
				res = teardown.TeardownPane(target, diagLogger(cmd))
			} else {
				// Team names flow into filesystem paths; reject path traversal /
				// separators / absolute paths via the typed constructor before
				// teardown runs.
				if _, err := ids.NewTeamID(target); err != nil {
					res = teardown.Result{
						OK:        false,
						Target:    target,
						ErrorCode: teardown.ErrCodeInternal,
						ErrorMsg:  err.Error(),
					}
				} else {
					res = teardown.TeardownTeam(target, diagLogger(cmd))
				}
			}
			return reportTeardown(res, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
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

	if res.OK {
		switch {
		case res.TeamRemoved:
			fmt.Printf("torn down team %q (panes: %d, members: %d)\n",
				res.Target, len(res.Panes), len(res.Members))
		case len(res.Panes) > 0 || len(res.Members) > 0:
			fmt.Printf("torn down pane %s (members: %v)\n",
				res.Target, res.Members)
		default:
			fmt.Printf("nothing to tear down for %q\n", res.Target)
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		return nil
	}

	fmt.Fprintf(os.Stderr, "teardown: %s: %s\n", res.ErrorCode, res.ErrorMsg)
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	os.Exit(1)
	return nil
}
