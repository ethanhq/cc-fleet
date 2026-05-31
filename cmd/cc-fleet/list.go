package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// listJSONEnvelope is the JSON shape `cc-fleet list --json` emits. Vendors
// is always non-nil even when empty so jq dispatch in the skill doesn't have
// to special-case nil.
type listJSONEnvelope struct {
	OK      bool                 `json:"ok"`
	Vendors []userops.VendorView `json:"vendors"`
}

func newListCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List configured vendors with status and cache info",
		Long: `List every vendor configured in vendors.toml in alphabetical order.

Each row shows the vendor name, current default model, enabled flag, secret
backend, the number of cached models, and a (stale) marker when the models
cache is older than 7 days (or missing entirely).

--json emits ` + "`{\"ok\":true,\"vendors\":[...]}`" + ` with one entry per vendor.
The list is always present (empty array for fresh installs) so the skill
can iterate without a presence check.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			res, err := userops.List()
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(listJSONEnvelope{OK: true, Vendors: res.Vendors})
				return nil
			}
			if len(res.Vendors) == 0 {
				fmt.Println("no vendors configured (run: cc-fleet add <vendor> ...)")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDEFAULT_MODEL\tSTATUS\tSECRET_BACKEND\tMODELS")
			for _, vv := range res.Vendors {
				status := "enabled"
				if !vv.Enabled {
					status = "disabled"
				}
				models := fmt.Sprintf("%d", vv.ModelsCount)
				if vv.ModelsStale {
					models += " (stale)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					vv.Name, vv.DefaultModel, status, vv.SecretBackend, models)
			}
			return w.Flush()
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
