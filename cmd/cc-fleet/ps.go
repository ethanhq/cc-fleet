package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// psEnvelope is the JSON shape `cc-fleet ps --json` emits. The teammates
// slice is always serialized (even when empty) so skill code can
// .teammates without a presence check.
type psEnvelope struct {
	OK        bool                `json:"ok"`
	Teammates []teardown.Teammate `json:"teammates"`
	Error     string              `json:"error,omitempty"`
}

// newPsCmd builds `cc-fleet ps [--json]` — list live cc-fleet teammates.
//
// "Live" = a claude process with --agent-id running inside some tmux
// pane. Manually launched claudes outside tmux are intentionally
// excluded; cc-fleet only owns processes it spawned.
func newPsCmd() *cobra.Command {
	var asJSON bool
	var check bool

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List live cc-fleet teammates",
		Long: `List every cc-fleet-spawned teammate currently running.

Identification: each row is a claude process with --agent-id that lives
inside a tmux pane (cc-fleet only spawns into panes). Teammates started
manually outside tmux are not listed.

Output: pretty table by default; --json emits {"ok":true,"teammates":[...]}
with one entry per live teammate. An empty fleet returns ok=true with an
empty array.

--check scans each teammate's tmux pane for provider API-error signatures
(429 / 401 / out-of-balance / rate limit) and adds a "status" field
(ok | error | unknown) plus "error_class" / "detail". Use it to detect a
provider teammate wedged in a retry loop — it never goes idle, so waiting on
an idle notification would block forever. The scan reports only the error
CLASS, never raw pane text (which can contain key fragments).`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPs(asJSON, check)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")
	cmd.Flags().BoolVar(&check, "check", false,
		"Scan each teammate's pane for API errors and add a status field")

	return cmd
}

func runPs(asJSON, check bool) error {
	teammates, err := teardown.DiscoverTeammates()
	if err != nil {
		if asJSON {
			env := psEnvelope{OK: false, Teammates: []teardown.Teammate{}, Error: err.Error()}
			data, mErr := json.Marshal(env)
			if mErr != nil {
				fmt.Fprintln(os.Stderr, "ps: marshal:", mErr)
				os.Exit(1)
			}
			fmt.Println(string(data))
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "ps:", err)
		os.Exit(1)
	}

	// Normalize nil → []Teammate{} so JSON consumers always see an array.
	if teammates == nil {
		teammates = []teardown.Teammate{}
	}

	// --check: enrich each row with a pane-scan health status. Opt-in so a
	// plain `ps` stays a cheap snapshot (no capture-pane exec per teammate).
	if check {
		teammates = teardown.AnnotateHealth(teammates)
	}

	if asJSON {
		env := psEnvelope{OK: true, Teammates: teammates}
		data, err := json.Marshal(env)
		if err != nil {
			fmt.Fprintln(os.Stderr, "ps: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(teammates) == 0 {
		fmt.Println("no live teammates")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if check {
		fmt.Fprintln(w, "NAME\tTEAM\tPANE\tPROVIDER\tMODEL\tPID\tSTATUS\tDETAIL")
		for _, t := range teammates {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				t.Name, t.Team, t.PaneID, t.Provider, t.Model, t.PID, t.Status, t.Detail)
		}
		return w.Flush()
	}
	fmt.Fprintln(w, "NAME\tTEAM\tPANE\tPROVIDER\tMODEL\tPID")
	for _, t := range teammates {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n",
			t.Name, t.Team, t.PaneID, t.Provider, t.Model, t.PID)
	}
	return w.Flush()
}
