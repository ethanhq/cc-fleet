package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// removeJSONEnvelope is the success-side JSON shape `cc-fleet remove --json`
// emits. `removed` echoes the provider name; `secret_removed` and
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
		Use:   "remove <provider>",
		Short: "Delete a provider and its profile (and optionally its secret)",
		Long: `Delete <provider> from providers.toml, remove its profile JSON, and (unless
--keep-secret is set) the credential it owns: a file-backend secret file, or
a codex provider's own cc-fleet login token plus its daemon.

Other external backends (pass, 1password, vault, keyring) and ~/.codex keep
their secrets untouched — remove those with the backend's own CLI if you no
longer want them.

Remove is idempotent at the filesystem level: a missing profile or secret
file is not an error. Removing a non-existent provider IS an error
(PROVIDER_UNKNOWN) so the skill doesn't silently drop typos.`,
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
					Removed:        res.Provider,
					SecretRemoved:  res.SecretRemoved,
					ProfileRemoved: res.ProfileRemoved,
				})
				return nil
			}
			fmt.Printf("removed provider %s (profile_removed=%v, secret_removed=%v)\n",
				res.Provider, res.ProfileRemoved, res.SecretRemoved)
			return nil
		},
	}

	cmd.Flags().BoolVar(&keepSecret, "keep-secret", false,
		"Preserve the credential cc-fleet would delete (a file-backend secret or a codex own-login)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}
