package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// repairJSONEnvelope is the JSON shape `cc-fleet repair --json` emits.
type repairJSONEnvelope struct {
	OK       bool     `json:"ok"`
	Repaired []string `json:"repaired"`
}

func newRepairCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Rewrite every vendor's profile JSON from vendors.toml",
		Long: `Rewrite ~/.claude/profiles/<vendor>.json for every vendor in
vendors.toml. Useful when:

  - a profile file was accidentally deleted
  - the cc-fleet binary moved (apiKeyHelper path needs to be re-pinned)
  - profile permissions drifted

Repair does NOT modify vendors.toml, secrets, or the models cache. It is
safe to re-run.`,
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
				emitJSON(repairJSONEnvelope{OK: true, Repaired: res.Repaired})
				return nil
			}
			if len(res.Repaired) == 0 {
				fmt.Println("no vendors to repair")
				return nil
			}
			fmt.Printf("repaired %d vendor profile(s): %v\n", len(res.Repaired), res.Repaired)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
