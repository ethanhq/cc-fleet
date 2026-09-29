package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// repairJSONEnvelope is the JSON shape `cc-fleet repair --json` emits.
type repairJSONEnvelope struct {
	OK           bool                 `json:"ok"`
	Repaired     []string             `json:"repaired"`
	Shim         string               `json:"shim,omitempty"`
	ShimRepinned bool                 `json:"shim_repinned"`
	AgentDefs    *teammate.SyncResult `json:"agent_defs,omitempty"`
}

func newRepairCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Rewrite every provider's profile JSON and re-pin the teammate launcher",
		Long: `Rewrite ~/.claude/profiles/<provider>.json for every provider in
providers.toml. Useful when:

  - a profile file was accidentally deleted
  - the cc-fleet binary moved (apiKeyHelper path needs to be re-pinned)
  - profile permissions drifted

For provider teammates it also re-pins the launcher shim
(~/.config/cc-fleet/bin/claude-teammate) to this binary and restores its
mode when the teammate lane is enabled or the shim exists, and resyncs the
ccf-* agent definitions when the lane is enabled.

Repair does NOT modify providers.toml, secrets, the models cache, or Claude
Code's settings.json. It is safe to re-run.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			res, err := userops.Repair()
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(repairJSONEnvelope{OK: true, Repaired: res.Repaired, Shim: res.Shim,
					ShimRepinned: res.ShimRepinned, AgentDefs: res.AgentDefs})
				return nil
			}
			if len(res.Repaired) == 0 {
				fmt.Println("no providers to repair")
			} else {
				fmt.Printf("repaired %d provider profile(s): %v\n", len(res.Repaired), res.Repaired)
			}
			if res.ShimRepinned {
				fmt.Printf("re-pinned the teammate launcher %s\n", res.Shim)
			}
			if d := res.AgentDefs; d != nil && len(d.Written)+len(d.Removed) > 0 {
				fmt.Printf("synced teammate agent definitions: wrote %v, removed %v\n", d.Written, d.Removed)
			}
			if d := res.AgentDefs; d != nil {
				if w := userops.AgentDefConflictWarning(d.Conflicts); w != "" {
					fmt.Println("warning: " + w)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
