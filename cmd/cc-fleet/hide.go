package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/panevis"
)

// newHideCmd builds `cc-fleet hide <target> [--json]` — hide a teammate's tmux
// pane (move it to the detached claude-hidden session) without killing the
// process. Follows teardown.go's --json / SilenceErrors discipline.
func newHideCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "hide <team|team/member|name@team|%pane>",
		Short: "Hide a teammate's tmux pane without killing it",
		Long: `Move a teammate's tmux pane into a detached hidden session so it
disappears from the visible layout — the process keeps running and can be
restored with "cc-fleet show".

The target may be a tmux pane id (%42), a "team/member", a "name@team" agent id,
or a bare team name (hides every member of the team that has a pane). The origin
window is recorded so show can put the pane back where it was.

Idempotent: hiding an already-hidden pane returns ok. In-tmux teammates only —
an out-of-tmux swarm teammate returns SWARM_UNSUPPORTED (it runs on a detached
server you aren't attached to; use the spawn's attach_command to view it).
--json emits the panevis.Result (single object, or an array when the target
expands to several members); exit 1 if any member failed.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPaneVis(args[0], asJSON, true)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")
	return cmd
}

// runPaneVis resolves target to (team,member) pairs and runs Hide (hide=true)
// or Show on each, then reports the aggregate.
func runPaneVis(target string, asJSON, hide bool) error {
	action := "show"
	if hide {
		action = "hide"
	}

	if onWindows {
		bad := panevis.Result{Action: action, ErrorCode: "UNSUPPORTED_ON_WINDOWS", ErrorMsg: windowsUnsupportedMsg(action)}
		return reportPaneVis([]panevis.Result{bad}, asJSON)
	}

	targets, err := panevis.Resolve(target)
	if err != nil {
		// Resolve carries the real code (TEAM_NOT_FOUND / PANE_NOT_FOUND / …) in a
		// ResolveError; fall back to BAD_ARGS only for an uncoded error.
		code := panevis.ErrBadArgs
		var re *panevis.ResolveError
		if errors.As(err, &re) {
			code = re.Code
		}
		bad := panevis.Result{Action: action, ErrorCode: code, ErrorMsg: err.Error()}
		return reportPaneVis([]panevis.Result{bad}, asJSON)
	}

	results := make([]panevis.Result, 0, len(targets))
	for _, tg := range targets {
		// Route through HideRef/ShowRef with the resolved socket + config pane id
		// so a swarm teammate's pane (private socket cc-fleet-swarm-<team>) is
		// acted on against the right tmux server, not the default one.
		if hide {
			results = append(results, panevis.HideRef(tg.Team, tg.Name, tg.Socket, tg.PaneID))
		} else {
			results = append(results, panevis.ShowRef(tg.Team, tg.Name, tg.Socket, tg.PaneID))
		}
	}
	return reportPaneVis(results, asJSON)
}

// reportPaneVis formats hide/show results. JSON mode emits a single Result
// object for one target (the common %pane / team/member case) or an array for
// many, then exits via os.Exit so cobra's error echo can't append a second line
// that breaks JSON parsers. Any failed member flips the exit code to 1.
func reportPaneVis(results []panevis.Result, asJSON bool) error {
	anyFail := false
	for _, r := range results {
		if !r.OK {
			anyFail = true
		}
	}

	if asJSON {
		var payload any = results
		if len(results) == 1 {
			payload = results[0]
		}
		data, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hide/show: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		if anyFail {
			os.Exit(1)
		}
		return nil
	}

	if len(results) == 0 {
		fmt.Println("nothing to do (no matching teammates with a pane)")
		return nil
	}
	for _, r := range results {
		if r.OK {
			fmt.Printf("%s %s/%s: ok (hidden=%v)\n", r.Action, r.Team, r.Name, r.Hidden)
		} else {
			fmt.Fprintf(os.Stderr, "%s %s/%s: %s: %s\n", r.Action, r.Team, r.Name, r.ErrorCode, r.ErrorMsg)
			if r.Suggestion != "" {
				fmt.Fprintln(os.Stderr, "  suggestion:", r.Suggestion)
			}
		}
	}
	if anyFail {
		os.Exit(1)
	}
	return nil
}
