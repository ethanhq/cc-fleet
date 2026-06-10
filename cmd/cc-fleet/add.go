package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// readKeyFromStdin reads the entire stdin to EOF as the API key bytes (trailing
// newline trimmed). The safe path: the key never enters argv (ps -ef / shell
// history) and never echoes — the caller pipes it via `--api-key-stdin` / heredoc.
//
// An empty key is rejected so a stray empty stdin doesn't silently install a
// blank secret file.
func readKeyFromStdin() (string, error) {
	b, err := readAllFromFd(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read --api-key-stdin: %w", err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("--api-key-stdin received empty input")
	}
	return s, nil
}

// readKeyFromFile reads the API key from path. The file must be mode <= 0600 so
// a key bag accidentally placed in a world-readable location is rejected up
// front. Trailing newline is trimmed.
func readKeyFromFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat --api-key-file: %w", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("--api-key-file %s has insecure mode %#o; chmod 600 first", path, mode)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read --api-key-file: %w", err)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("--api-key-file is empty")
	}
	return s, nil
}

// addJSONEnvelope is the success-side JSON shape `cc-fleet add --json` emits.
type addJSONEnvelope struct {
	OK          bool      `json:"ok"`
	Provider    string    `json:"provider"`
	ProfilePath string    `json:"profile_path"`
	AddedAt     time.Time `json:"added_at"`
	ModelCount  int       `json:"model_count"`
}

func newAddCmd() *cobra.Command {
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
		disabled       bool
		asJSON         bool
	)

	cmd := &cobra.Command{
		Use:   "add <provider>",
		Short: "Register a provider and probe its /v1/models endpoint",
		Long: `Register a provider in providers.toml and write its Claude Code profile JSON.

The provider name must match ^[a-zA-Z][a-zA-Z0-9_-]{0,31}$. Existing entries
are NOT overwritten — use ` + "`cc-fleet edit`" + ` or ` + "`cc-fleet remove`" + ` first.

Add performs a synchronous probe of the provider's models_endpoint with a 3s
timeout. The probe MUST succeed before the provider is persisted:

  KEY_INVALID         provider returned HTTP 401         (exit 1)
  PROVIDER_UNREACHABLE  DNS / connect / HTTP timeout    (exit 1)
  ADD_FAILED          anything else                    (exit 1)

If --api-key is supplied with --secret-backend file, the key is written to
~/.config/cc-fleet/secrets/<secret-ref> at mode 0600 BEFORE the probe; on
failure that file is rolled back so re-running add with a fresh key is
safe. --api-key is rejected for any non-file backend (use the backend's own
CLI to provision the secret).

Without --json, missing required flags are filled in by interactive prompts
when stdin is a tty; otherwise the command exits 1 with a usage error.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Resolve the API key from the safest available source. --api-key-stdin
			// / --api-key-file keep the key out of argv (and so out of `ps`, shell
			// history, transcripts). Plain --api-key remains for compatibility but
			// emits a one-shot stderr warning. The three flags are mutually exclusive.
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
				reportUserOpErr(asJSON, err)
				return err
			}
			if apiKeyStdin {
				k, err := readKeyFromStdin()
				if err != nil {
					reportUserOpErr(asJSON, err)
					return err
				}
				apiKey = k
			} else if apiKeyFile != "" {
				k, err := readKeyFromFile(apiKeyFile)
				if err != nil {
					reportUserOpErr(asJSON, err)
					return err
				}
				apiKey = k
			} else if apiKey != "" {
				// One-time stderr warning so a long-running interactive shell
				// session sees it once instead of every command.
				fmt.Fprintln(os.Stderr,
					"cc-fleet: warning: --api-key <value> is DEPRECATED; "+
						"the key enters process argv and shell history. "+
						"Use --api-key-stdin (heredoc / pipe) or --api-key-file <path> instead.")
			}

			req := userops.AddRequest{
				Name:           args[0],
				BaseURL:        baseURL,
				ModelsEndpoint: modelsEndpoint,
				DefaultModel:   defaultModel,
				StrongModel:    strongModel,
				FastModel:      fastModel,
				Effort:         effort,
				DefaultPerm:    defaultPerm,
				SecretBackend:  secretBackend,
				SecretRef:      secretRef,
				APIKey:         apiKey,
				Enabled:        !disabled,
			}
			if err := fillAddDefaults(&req, asJSON); err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			res, err := userops.Add(req)
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(addJSONEnvelope{
					OK:          true,
					Provider:    res.Provider,
					ProfilePath: res.ProfilePath,
					AddedAt:     res.AddedAt,
					ModelCount:  res.ModelCount,
				})
				return nil
			}
			fmt.Printf("added provider %s (profile: %s, models: %d)\n",
				res.Provider, res.ProfilePath, res.ModelCount)
			return nil
		},
	}

	cmd.Flags().StringVar(&baseURL, "base-url", "",
		"Provider base URL written into the profile's ANTHROPIC_BASE_URL env var (required)")
	cmd.Flags().StringVar(&modelsEndpoint, "models-endpoint", "",
		"Provider /v1/models URL used for probe + cc-fleet refresh (required)")
	cmd.Flags().StringVar(&defaultModel, "default-model", "",
		"Model id passed to spawn when --model is omitted (required)")
	cmd.Flags().StringVar(&strongModel, "strong-model", "",
		"Optional model id for the 'strong' slot (blank → follows --default-model)")
	cmd.Flags().StringVar(&fastModel, "fast-model", "",
		"Optional model id for the 'fast'/background slot (blank → follows --default-model)")
	cmd.Flags().StringVar(&effort, "effort", "",
		"Optional reasoning-effort level (low|medium|high|xhigh|max); needs provider support")
	cmd.Flags().StringVar(&defaultPerm, "default-permission", "",
		"Optional default permission mode for `cc-fleet run` (default|acceptEdits|plan|auto|bypassPermissions)")
	cmd.Flags().StringVar(&secretBackend, "secret-backend", "file",
		"Where to fetch the API key (file|pass|1password|vault|keyring; default: file)")
	cmd.Flags().StringVar(&secretRef, "secret-ref", "",
		"Reference passed to the backend (filename for file, path for pass, etc.) (required)")
	cmd.Flags().StringVar(&apiKey, "api-key", "",
		"DEPRECATED — enters argv/history. Use --api-key-stdin or --api-key-file. (file backend only)")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false,
		"Read the API key from stdin until EOF (safer than --api-key). file backend only.")
	cmd.Flags().StringVar(&apiKeyFile, "api-key-file", "",
		"Read the API key from this file (mode must be <= 0600). file backend only.")
	cmd.Flags().BoolVar(&disabled, "disabled", false,
		"Add the provider in the disabled state (default: enabled)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

// fillAddDefaults fills required fields in req via interactive prompts when
// stdin is a tty. In --json mode (or on non-tty stdin), missing required
// fields are a hard error so the skill never blocks waiting for input.
func fillAddDefaults(req *userops.AddRequest, asJSON bool) error {
	// Required: base_url, models_endpoint, default_model, secret_ref.
	missing := missingRequired(req)
	if len(missing) == 0 {
		return nil
	}
	if asJSON || !isTTY(os.Stdin) {
		return &userops.Op{
			Code: userops.CodeAddFailed,
			Err: fmt.Errorf("missing required flag(s) %s (and stdin is not a tty)",
				strings.Join(missing, ", ")),
		}
	}
	reader := bufio.NewReader(os.Stdin)
	if req.BaseURL == "" {
		v, err := promptLine(reader, "  base_url: ")
		if err != nil {
			return err
		}
		req.BaseURL = v
	}
	if req.ModelsEndpoint == "" {
		v, err := promptLine(reader, "  models_endpoint: ")
		if err != nil {
			return err
		}
		req.ModelsEndpoint = v
	}
	if req.DefaultModel == "" {
		v, err := promptLine(reader, "  default_model: ")
		if err != nil {
			return err
		}
		req.DefaultModel = v
	}
	if req.SecretRef == "" {
		v, err := promptLine(reader, "  secret_ref (e.g. "+req.Name+".key): ")
		if err != nil {
			return err
		}
		if v == "" {
			v = req.Name + ".key"
		}
		req.SecretRef = v
	}
	// Only file backend allows the inline --api-key path; for it, prompt for the
	// key value too if the user didn't supply --api-key. The prompt uses password-
	// style (no-echo) input so the key never hits terminal scrollback.
	if req.SecretBackend == "file" && req.APIKey == "" {
		v, err := promptPassword("  API key (stored at <secret-ref>): ")
		if err != nil {
			return err
		}
		req.APIKey = v
	}
	_ = reader // reader still used above for other prompts; silence unused if future refactor reorders
	if still := missingRequired(req); len(still) != 0 {
		return errors.New("required field(s) still missing after prompt: " + strings.Join(still, ", "))
	}
	return nil
}

// missingRequired returns the names of required fields that are still empty
// in req. SecretBackend has a default value (file), so it's never "missing".
func missingRequired(req *userops.AddRequest) []string {
	var out []string
	if req.BaseURL == "" {
		out = append(out, "--base-url")
	}
	if req.ModelsEndpoint == "" {
		out = append(out, "--models-endpoint")
	}
	if req.DefaultModel == "" {
		out = append(out, "--default-model")
	}
	if req.SecretRef == "" {
		out = append(out, "--secret-ref")
	}
	return out
}
