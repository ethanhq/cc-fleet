package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// exportJSONEnvelope is the JSON shape `cc-fleet export --json` emits. With --out the
// bundle is written to the file and Out names it; without --out the bundle TOML is
// embedded as a string so stdout never mixes TOML and JSON.
type exportJSONEnvelope struct {
	OK           bool     `json:"ok"`
	Written      []string `json:"written"`
	OmittedCodex []string `json:"omitted_codex"`
	Out          string   `json:"out,omitempty"`
	Bundle       string   `json:"bundle,omitempty"`
}

func newExportCmd() *cobra.Command {
	var out string
	var providers []string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write a keyless TOML bundle of the provider roster for another machine",
		Long: `Write a versioned, human-readable TOML bundle of the provider roster (config
+ the global default) for import on another machine. The bundle carries NO API key:
file-backend keys stay on this machine; pass/1password/vault/keyring rows carry only
their reference. codex providers are omitted (their login is machine-local).

  cc-fleet export --out fleet-providers.toml
  cc-fleet export --provider deepseek,glm > fleet-providers.toml

Re-enter keys on the target with ` + "`cc-fleet edit --api-key-stdin/--api-key-file`" + ` and
re-run ` + "`cc-fleet codex login`" + ` for any codex providers.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			res, err := userops.Export(userops.ExportRequest{Providers: providers})
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}

			if asJSON {
				env := exportJSONEnvelope{OK: true, Written: res.Written, OmittedCodex: res.OmittedCodex}
				if out != "" {
					if err := os.WriteFile(out, res.Bundle, 0o600); err != nil {
						reportUserOpErr(true, err)
						return err
					}
					env.Out = out
				} else {
					env.Bundle = string(res.Bundle)
				}
				return json.NewEncoder(os.Stdout).Encode(env)
			}

			if out != "" {
				if err := os.WriteFile(out, res.Bundle, 0o600); err != nil {
					reportUserOpErr(false, err)
					return err
				}
				fmt.Fprintf(os.Stderr, "wrote %d provider(s) to %s\n", len(res.Written), out)
			} else {
				_, _ = os.Stdout.Write(res.Bundle)
			}
			if len(res.OmittedCodex) > 0 {
				fmt.Fprintf(os.Stderr, "omitted codex providers (re-add + `codex login` on the target): %v\n", res.OmittedCodex)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&out, "out", "", "write the bundle to this file (default: stdout)")
	cmd.Flags().StringSliceVar(&providers, "provider", nil, "export only these providers (comma-separated)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON result envelope")
	return cmd
}
