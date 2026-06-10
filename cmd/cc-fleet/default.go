package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// defaultJSONEnvelope is the JSON shape `cc-fleet default [--json]` emits (show
// and set/unset both return the resulting view).
type defaultJSONEnvelope struct {
	OK         bool     `json:"ok"`
	Provider   string   `json:"provider"`
	Source     string   `json:"source"`     // configured | auto | unset
	Configured string   `json:"configured"` // the pinned value ("" = not pinned)
	Candidates []string `json:"candidates"`
}

func newDefaultCmd() *cobra.Command {
	var asJSON, unset, force bool

	cmd := &cobra.Command{
		Use:   "default [provider]",
		Short: "Show or set the default provider used when a lane omits one",
		Long: `Without arguments, show the effective default provider: the one pinned in
providers.toml (source "configured"), or — when none is pinned and exactly one
provider is enabled — that sole provider (source "auto"), or "unset".

With a <provider> argument, pin it as the default. Pinning refuses to overwrite
an existing default unless --force is given. --unset clears the pin.

The default is the provider a provider-less spawn / subagent / run / workflow
agent() resolves to; the model is still chosen per call from that provider's
default/strong/fast roster.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			var (
				view *userops.DefaultProviderView
				err  error
			)
			switch {
			case unset:
				view, err = userops.UnsetDefaultProvider()
			case len(args) == 1:
				view, err = userops.SetDefaultProvider(args[0], force)
			default:
				view, err = userops.DefaultProvider()
			}
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(defaultJSONEnvelope{
					OK:         true,
					Provider:   view.Provider,
					Source:     view.Source,
					Configured: view.Configured,
					Candidates: view.Candidates,
				})
				return nil
			}
			switch view.Source {
			case "configured":
				fmt.Printf("default provider: %s (configured)\n", view.Provider)
			case "auto":
				fmt.Printf("default provider: %s (auto — the only enabled provider)\n", view.Provider)
			case "disabled":
				fmt.Printf("default provider: %s is configured but DISABLED — re-enable it, or `cc-fleet default <provider> --force`, or `cc-fleet default --unset`\n", view.Provider)
			case "unknown":
				fmt.Printf("default provider: %s is configured but no longer exists — `cc-fleet default <provider> --force`, or `cc-fleet default --unset`\n", view.Provider)
			default:
				if len(view.Candidates) == 0 {
					fmt.Println("default provider: unset (no providers configured — run: cc-fleet add <provider> ...)")
				} else {
					fmt.Printf("default provider: unset (enabled: %s — pin one with: cc-fleet default <provider>)\n",
						strings.Join(view.Candidates, ", "))
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	cmd.Flags().BoolVar(&unset, "unset", false, "Clear the pinned default provider")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an already-set default")
	return cmd
}
