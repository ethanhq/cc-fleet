package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// modelRole is one configured capability slot of a provider's roster.
type modelRole struct {
	Role string `json:"role"` // default | strong | fast
	ID   string `json:"id"`   // the model id passed to claude (may carry the [1m] marker)
}

// modelsEnvelope is the JSON shape `cc-fleet models <provider> --json` emits: the
// provider's configured capability roster (default/strong/fast), NOT the full
// upstream catalog. The full /v1/models list stays in the cache for the TUI model
// picker; the agent-facing command shows only the few configured slots so a
// provider with a large catalog can't flood the orchestrator. Models is always
// serialized (even when empty) so skill code can iterate without a presence check.
type modelsEnvelope struct {
	OK         bool        `json:"ok"`
	Provider   string      `json:"provider"`
	Models     []modelRole `json:"models"`
	Error      string      `json:"error,omitempty"`
	ErrorCode  string      `json:"error_code,omitempty"`
	Suggestion string      `json:"suggestion,omitempty"`
}

// Error codes for `cc-fleet models <provider>` — kept stable so the skill can
// dispatch on them without prose parsing.
const (
	codeModelsProviderUnknown  = "PROVIDER_UNKNOWN"
	codeModelsConfigLoadFailed = "CONFIG_LOAD_FAILED"
)

func newModelsCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "models <provider>",
		Short: "List a provider's configured model roster (default/strong/fast)",
		Long: `List the configured capability roster for <provider>: the default, strong, and
fast model slots from providers.toml (a blank strong/fast slot follows the
default). This is intentionally NOT the full ` + "`/v1/models`" + ` catalog — that
list is only used by the TUI model picker; ` + "`cc-fleet models`" + ` shows just the
few configured slots so a provider with a large catalog can't flood an agent.

Use ` + "`--model default|strong|fast`" + ` (or a literal id) on spawn/subagent/run to
select a slot. Use --json for skill consumption.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runModels(args[0], asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

func runModels(provider string, asJSON bool) error {
	cfg, err := config.Load()
	if err != nil {
		return reportModelsErr(asJSON, provider, codeModelsConfigLoadFailed,
			fmt.Errorf("load providers.toml: %w", err),
			"check ~/.config/cc-fleet/providers.toml")
	}

	v, ok := cfg.Providers[provider]
	if !ok {
		return reportModelsErr(asJSON, provider, codeModelsProviderUnknown,
			fmt.Errorf("provider %q not in providers.toml (run: cc-fleet list)", provider),
			"cc-fleet list")
	}

	roster := []modelRole{
		{Role: config.ModelSlotDefault, ID: v.DefaultModel},
		{Role: config.ModelSlotStrong, ID: v.StrongModelOrDefault()},
		{Role: config.ModelSlotFast, ID: v.FastModelOrDefault()},
	}

	if asJSON {
		env := modelsEnvelope{OK: true, Provider: v.Name, Models: roster}
		data, mErr := json.Marshal(env)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "models: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("provider %s — configured model roster:\n", v.Name)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ROLE\tMODEL")
	for _, m := range roster {
		fmt.Fprintf(w, "%s\t%s\n", m.Role, m.ID)
	}
	return w.Flush()
}

// reportModelsErr writes the failure envelope (JSON or pretty) and exits
// non-zero so the skill never sees a half-line. Returns nil only because
// the os.Exit call won't return — keeps the signature consistent with
// reportSpawn / reportTeardown.
func reportModelsErr(asJSON bool, provider, code string, err error, suggestion string) error {
	if asJSON {
		env := modelsEnvelope{
			OK:         false,
			Provider:   provider,
			Models:     []modelRole{},
			Error:      err.Error(),
			ErrorCode:  code,
			Suggestion: suggestion,
		}
		data, mErr := json.Marshal(env)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "models: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "models: %s: %s\n", code, err)
	if suggestion != "" {
		fmt.Fprintln(os.Stderr, "suggestion:", suggestion)
	}
	os.Exit(1)
	return nil
}
