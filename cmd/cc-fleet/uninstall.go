package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// uninstallJSONEnvelope is the JSON shape `cc-fleet uninstall --json` emits.
// Removed enumerates the paths that were deleted; Kept enumerates the paths
// we deliberately left alone (or failed to remove and surfaced as a soft
// note rather than an error).
type uninstallJSONEnvelope struct {
	OK      bool     `json:"ok"`
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
}

func newUninstallCmd() *cobra.Command {
	var (
		keepSecrets bool
		wipeSecrets bool
		asJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove all cc-fleet config + cached state",
		Long: `Remove every file cc-fleet manages on disk:

  ~/.claude/profiles/<vendor>.json   (one per vendor)
  ~/.config/cc-fleet/vendors.toml
  ~/.config/cc-fleet/fingerprint.json
  ~/.config/cc-fleet/models-cache.json
  ~/.config/cc-fleet/subagent-jobs/  (finished background subagent jobs)

Per-vendor file-backend secrets in ~/.config/cc-fleet/secrets/ are
preserved by default (--keep-secrets, the default). Pass --wipe-secrets to
remove the entire secrets/ directory.

Background subagent jobs that are still running are left intact (with a
note on stderr) so uninstall never yanks files from a live job; reap them
later with ` + "`cc-fleet subagent-gc`" + ` once they finish (or just re-run uninstall).

We deliberately do NOT touch:
  ~/.claude/skills/vendor-fleet/   (owned by install machinery)
  ~/.claude/teams/                    (owned by Claude Code itself)

Uninstall is idempotent — missing files are not an error. After uninstall
you can run ` + "`cc-fleet init`" + ` again to start over.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Both flags are only a real conflict when the user explicitly
			// set --keep-secrets=true AND --wipe-secrets — bare --wipe-secrets
			// on its own is fine even though --keep-secrets defaults to true.
			if cmd.Flags().Changed("keep-secrets") && wipeSecrets && keepSecrets {
				err := fmt.Errorf("--keep-secrets and --wipe-secrets are mutually exclusive")
				reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeUninstallFailed, Err: err})
				return err
			}
			// keep wins unless wipe is set; explicit --keep-secrets=false also wipes.
			keep := keepSecrets && !wipeSecrets
			req := userops.UninstallRequest{KeepSecrets: keep}
			res, err := userops.Uninstall(req)
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(uninstallJSONEnvelope{
					OK:      true,
					Removed: res.Removed,
					Kept:    res.Kept,
				})
				return nil
			}
			fmt.Printf("uninstalled cc-fleet (removed %d path(s), kept %d)\n",
				len(res.Removed), len(res.Kept))
			for _, p := range res.Removed {
				fmt.Println("  removed:", p)
			}
			for _, p := range res.Kept {
				fmt.Println("  kept:   ", p)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&keepSecrets, "keep-secrets", true,
		"Preserve ~/.config/cc-fleet/secrets/ (default)")
	cmd.Flags().BoolVar(&wipeSecrets, "wipe-secrets", false,
		"Remove ~/.config/cc-fleet/secrets/ entirely (overrides --keep-secrets)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
