package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// removeJSONEnvelope is the success-side JSON shape `cc-fleet remove --json`
// emits. `removed` echoes the vendor name; `secret_removed` and
// `profile_removed` let the skill confirm side-effects without re-reading
// the filesystem.
type removeJSONEnvelope struct {
	OK             bool   `json:"ok"`
	Removed        string `json:"removed"`
	SecretRemoved  bool   `json:"secret_removed"`
	ProfileRemoved bool   `json:"profile_removed"`
}

func newRemoveCmd() *cobra.Command {
	var (
		keepSecret bool
		asJSON     bool
	)

	cmd := &cobra.Command{
		Use:   "remove <vendor>",
		Short: "Delete a vendor and its profile (and optionally its secret)",
		Long: `Delete <vendor> from vendors.toml, remove its profile JSON, and (for
file-backend vendors) delete its secret file unless --keep-secret is set.

Non-file backends (pass, 1password, vault, keyring) keep their secrets
untouched — remove the secret with the backend's own CLI if you no longer
want it.

Remove is idempotent at the filesystem level: a missing profile or secret
file is not an error. Removing a non-existent vendor IS an error
(VENDOR_UNKNOWN) so the skill doesn't silently drop typos.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			res, err := userops.Remove(userops.RemoveRequest{
				Name:       args[0],
				KeepSecret: keepSecret,
			})
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(removeJSONEnvelope{
					OK:             true,
					Removed:        res.Vendor,
					SecretRemoved:  res.SecretRemoved,
					ProfileRemoved: res.ProfileRemoved,
				})
				return nil
			}
			fmt.Printf("removed vendor %s (profile_removed=%v, secret_removed=%v)\n",
				res.Vendor, res.ProfileRemoved, res.SecretRemoved)
			return nil
		},
	}

	cmd.Flags().BoolVar(&keepSecret, "keep-secret", false,
		"Don't delete the file-backend secret (no-op for non-file backends)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
