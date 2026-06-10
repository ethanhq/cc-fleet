package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// editProviderView is the JSON shape we emit for the modified provider. We don't
// re-use config.Provider directly because its struct fields only carry TOML
// tags (Go's encoding/json would otherwise capitalize keys, diverging from
// `cc-fleet list --json`'s shape).
type editProviderView struct {
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	DefaultModel   string `json:"default_model"`
	StrongModel    string `json:"strong_model,omitempty"`
	FastModel      string `json:"fast_model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	DefaultPerm    string `json:"default_permission,omitempty"`
	ModelsEndpoint string `json:"models_endpoint"`
	SecretBackend  string `json:"secret_backend"`
	SecretRef      string `json:"secret_ref"`
	Enabled        bool   `json:"enabled"`
	// KeyRotation is omitted when off/empty so single-key providers' JSON shape stays tight.
	KeyRotation string `json:"key_rotation,omitempty"`
}

// editJSONEnvelope is the success-side JSON shape `cc-fleet edit --json`
// emits. The full post-edit provider row is included so skill consumers can
// observe the new state without re-running list.
type editJSONEnvelope struct {
	OK       bool             `json:"ok"`
	Provider editProviderView `json:"provider"`
}

func newEditCmd() *cobra.Command {
	var (
		baseURL        string
		modelsEndpoint string
		defaultModel   string
		strongModel    string
		fastModel      string
		effort         string
		defaultPerm    string
		secretBackend  string
		secretRef      string
		apiKey         string
		apiKeyStdin    bool
		apiKeyFile     string
		keyRotation    string
		enable         bool
		disable        bool
		asJSON         bool
	)

	cmd := &cobra.Command{
		Use:   "edit <provider>",
		Short: "Modify selected fields on an existing provider",
		Long: `Modify an existing provider in providers.toml. Only flags you pass are
applied; everything else is preserved.

  --base-url            Update the ANTHROPIC_BASE_URL in the profile JSON too
  --models-endpoint     Update /v1/models URL used by cc-fleet refresh
  --default-model       Update the model used when spawn omits --model
  --secret-backend      Switch secret backend (file|pass|1password|vault|keyring)
  --secret-ref          Switch the reference used by the secret backend
  --api-key             Rotate the key (file backend only; writes to the
                        provider's existing secret_ref)
  --key-rotation        Per-worker multi-key rotation (off|round_robin|random)
  --enable / --disable  Flip the enabled flag (mutually exclusive)

Edit does NOT probe the provider — use ` + "`cc-fleet refresh <provider>`" + ` after
changing a URL or key to revalidate.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			if enable && disable {
				err := fmt.Errorf("--enable and --disable are mutually exclusive")
				reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeAddFailed, Err: err})
				return err
			}
			// Resolve the API key from the safest available source (same rules as
			// add: mutually exclusive, file mode 0600, deprecation warning for
			// inline --api-key).
			used := 0
			if apiKey != "" {
				used++
			}
			if apiKeyStdin {
				used++
			}
			if apiKeyFile != "" {
				used++
			}
			if used > 1 {
				err := errors.New("--api-key, --api-key-stdin, and --api-key-file are mutually exclusive")
				reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeAddFailed, Err: err})
				return err
			}
			if apiKeyStdin {
				k, err := readKeyFromStdin()
				if err != nil {
					reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeAddFailed, Err: err})
					return err
				}
				apiKey = k
			} else if apiKeyFile != "" {
				k, err := readKeyFromFile(apiKeyFile)
				if err != nil {
					reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeAddFailed, Err: err})
					return err
				}
				apiKey = k
			} else if apiKey != "" {
				fmt.Fprintln(os.Stderr,
					"cc-fleet: warning: --api-key <value> is DEPRECATED; "+
						"the key enters process argv and shell history. "+
						"Use --api-key-stdin (heredoc / pipe) or --api-key-file <path> instead.")
			}
			req := userops.EditRequest{Name: args[0]}
			if cmdHasFlag(baseURL) {
				req.BaseURL = &baseURL
			}
			if cmdHasFlag(modelsEndpoint) {
				req.ModelsEndpoint = &modelsEndpoint
			}
			if cmdHasFlag(defaultModel) {
				req.DefaultModel = &defaultModel
			}
			if cmdHasFlag(strongModel) {
				req.StrongModel = &strongModel
			}
			if cmdHasFlag(fastModel) {
				req.FastModel = &fastModel
			}
			if cmdHasFlag(effort) {
				req.Effort = &effort
			}
			if cmdHasFlag(defaultPerm) {
				req.DefaultPerm = &defaultPerm
			}
			if cmdHasFlag(secretBackend) {
				req.SecretBackend = &secretBackend
			}
			if cmdHasFlag(secretRef) {
				req.SecretRef = &secretRef
			}
			if cmdHasFlag(apiKey) {
				req.APIKey = apiKey
			}
			if cmdHasFlag(keyRotation) {
				req.KeyRotation = &keyRotation
			}
			if enable {
				b := true
				req.Enabled = &b
			} else if disable {
				b := false
				req.Enabled = &b
			}

			res, err := userops.Edit(req)
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(editJSONEnvelope{
					OK: true,
					Provider: editProviderView{
						Name:           res.Provider.Name,
						BaseURL:        res.Provider.BaseURL,
						DefaultModel:   res.Provider.DefaultModel,
						StrongModel:    res.Provider.StrongModel,
						FastModel:      res.Provider.FastModel,
						Effort:         res.Provider.Effort,
						DefaultPerm:    res.Provider.DefaultPermission,
						ModelsEndpoint: res.Provider.ModelsEndpoint,
						SecretBackend:  res.Provider.SecretBackend,
						SecretRef:      res.Provider.SecretRef,
						Enabled:        res.Provider.Enabled,
						KeyRotation:    res.Provider.KeyRotation,
					},
				})
				return nil
			}
			fmt.Printf("updated provider %s\n", res.Provider.Name)
			fmt.Printf("  base_url         = %s\n", res.Provider.BaseURL)
			fmt.Printf("  default_model    = %s\n", res.Provider.DefaultModel)
			if res.Provider.StrongModel != "" {
				fmt.Printf("  strong_model     = %s\n", res.Provider.StrongModel)
			}
			if res.Provider.FastModel != "" {
				fmt.Printf("  fast_model       = %s\n", res.Provider.FastModel)
			}
			if res.Provider.Effort != "" {
				fmt.Printf("  effort           = %s\n", res.Provider.Effort)
			}
			if res.Provider.DefaultPermission != "" {
				fmt.Printf("  default_perm     = %s\n", res.Provider.DefaultPermission)
			}
			fmt.Printf("  models_endpoint  = %s\n", res.Provider.ModelsEndpoint)
			fmt.Printf("  secret_backend   = %s\n", res.Provider.SecretBackend)
			fmt.Printf("  secret_ref       = %s\n", res.Provider.SecretRef)
			fmt.Printf("  enabled          = %v\n", res.Provider.Enabled)
			if res.Provider.KeyRotation != "" {
				fmt.Printf("  key_rotation     = %s\n", res.Provider.KeyRotation)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&baseURL, "base-url", "",
		"New ANTHROPIC_BASE_URL (rewrites profile JSON)")
	cmd.Flags().StringVar(&modelsEndpoint, "models-endpoint", "",
		"New /v1/models URL")
	cmd.Flags().StringVar(&defaultModel, "default-model", "",
		"New default model id")
	cmd.Flags().StringVar(&strongModel, "strong-model", "",
		"New 'strong' slot model id (empty arg = no change; clear it in the TUI)")
	cmd.Flags().StringVar(&fastModel, "fast-model", "",
		"New 'fast'/background slot model id (empty arg = no change; clear it in the TUI)")
	cmd.Flags().StringVar(&effort, "effort", "",
		"New reasoning-effort level (low|medium|high|xhigh|max; empty arg = no change)")
	cmd.Flags().StringVar(&defaultPerm, "default-permission", "",
		"New default permission mode for `cc-fleet run` (empty arg = no change)")
	cmd.Flags().StringVar(&secretBackend, "secret-backend", "",
		"New secret backend (file|pass|1password|vault|keyring)")
	cmd.Flags().StringVar(&secretRef, "secret-ref", "",
		"New secret reference")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"DEPRECATED — enters argv/history. Use --api-key-stdin or --api-key-file. (file backend only)")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false,
		"Read the new API key from stdin until EOF (safer than --api-key). file backend only.")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "",
		"Read the new API key from this file (mode must be <= 0600). file backend only.")
	cmd.Flags().StringVar(&keyRotation, "key-rotation", "",
		"Per-worker multi-key rotation strategy (off|round_robin|random)")
	cmd.Flags().BoolVar(&enable, "enable", false, "Set enabled=true")
	cmd.Flags().BoolVar(&disable, "disable", false, "Set enabled=false")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

// cmdHasFlag returns true iff the user actually passed a non-empty value for
// a string flag. We treat an empty string as "not set" rather than "set to
// empty"; the only fields where "" is meaningful are validated by config.
func cmdHasFlag(s string) bool { return s != "" }
