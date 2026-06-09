package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// editVendorView is the JSON shape we emit for the modified vendor. We don't
// re-use config.Vendor directly because its struct fields only carry TOML
// tags (Go's encoding/json would otherwise capitalize keys, diverging from
// `cc-fleet list --json`'s shape).
type editVendorView struct {
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
	// KeyRotation is omitted when off/empty so single-key vendors' JSON shape stays tight.
	KeyRotation string `json:"key_rotation,omitempty"`
}

// editJSONEnvelope is the success-side JSON shape `cc-fleet edit --json`
// emits. The full post-edit vendor row is included so skill consumers can
// observe the new state without re-running list.
type editJSONEnvelope struct {
	OK     bool           `json:"ok"`
	Vendor editVendorView `json:"vendor"`
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
		Use:   "edit <vendor>",
		Short: "Modify selected fields on an existing vendor",
		Long: `Modify an existing vendor in vendors.toml. Only flags you pass are
applied; everything else is preserved.

  --base-url            Update the ANTHROPIC_BASE_URL in the profile JSON too
  --models-endpoint     Update /v1/models URL used by cc-fleet refresh
  --default-model       Update the model used when spawn omits --model
  --secret-backend      Switch secret backend (file|pass|1password|vault|keyring)
  --secret-ref          Switch the reference used by the secret backend
  --api-key             Rotate the key (file backend only; writes to the
                        vendor's existing secret_ref)
  --key-rotation        Per-worker multi-key rotation (off|round_robin|random)
  --enable / --disable  Flip the enabled flag (mutually exclusive)

Edit does NOT probe the vendor — use ` + "`cc-fleet refresh <vendor>`" + ` after
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
					Vendor: editVendorView{
						Name:           res.Vendor.Name,
						BaseURL:        res.Vendor.BaseURL,
						DefaultModel:   res.Vendor.DefaultModel,
						StrongModel:    res.Vendor.StrongModel,
						FastModel:      res.Vendor.FastModel,
						Effort:         res.Vendor.Effort,
						DefaultPerm:    res.Vendor.DefaultPermission,
						ModelsEndpoint: res.Vendor.ModelsEndpoint,
						SecretBackend:  res.Vendor.SecretBackend,
						SecretRef:      res.Vendor.SecretRef,
						Enabled:        res.Vendor.Enabled,
						KeyRotation:    res.Vendor.KeyRotation,
					},
				})
				return nil
			}
			fmt.Printf("updated vendor %s\n", res.Vendor.Name)
			fmt.Printf("  base_url         = %s\n", res.Vendor.BaseURL)
			fmt.Printf("  default_model    = %s\n", res.Vendor.DefaultModel)
			if res.Vendor.StrongModel != "" {
				fmt.Printf("  strong_model     = %s\n", res.Vendor.StrongModel)
			}
			if res.Vendor.FastModel != "" {
				fmt.Printf("  fast_model       = %s\n", res.Vendor.FastModel)
			}
			if res.Vendor.Effort != "" {
				fmt.Printf("  effort           = %s\n", res.Vendor.Effort)
			}
			if res.Vendor.DefaultPermission != "" {
				fmt.Printf("  default_perm     = %s\n", res.Vendor.DefaultPermission)
			}
			fmt.Printf("  models_endpoint  = %s\n", res.Vendor.ModelsEndpoint)
			fmt.Printf("  secret_backend   = %s\n", res.Vendor.SecretBackend)
			fmt.Printf("  secret_ref       = %s\n", res.Vendor.SecretRef)
			fmt.Printf("  enabled          = %v\n", res.Vendor.Enabled)
			if res.Vendor.KeyRotation != "" {
				fmt.Printf("  key_rotation     = %s\n", res.Vendor.KeyRotation)
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
