package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/models"
)

// modelsEnvelope is the JSON shape `cc-fleet models <vendor> --json` emits.
// Models is always serialized (even when empty) so skill code can iterate
// without a presence check.
type modelsEnvelope struct {
	OK         bool           `json:"ok"`
	Vendor     string         `json:"vendor"`
	Endpoint   string         `json:"endpoint,omitempty"`
	FetchedAt  time.Time      `json:"fetched_at,omitempty"`
	Stale      bool           `json:"stale"`
	Models     []models.Model `json:"models"`
	Error      string         `json:"error,omitempty"`
	ErrorCode  string         `json:"error_code,omitempty"`
	Suggestion string         `json:"suggestion,omitempty"`
}

// Error codes for `cc-fleet models <vendor>` — kept stable so the skill can
// dispatch on them without prose parsing.
const (
	codeModelsVendorNotInCache = "VENDOR_NOT_IN_CACHE"
	codeModelsCacheLoadFailed  = "CACHE_LOAD_FAILED"
)

func newModelsCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "models <vendor>",
		Short: "List cached models for a vendor",
		Long: `List models in the local cache for <vendor>.

The cache is populated by ` + "`cc-fleet refresh <vendor>`" + `. If <vendor>
is not in the cache, the command reports an error and tells the user to
run refresh. Cache entries older than ~/.config/cc-fleet/models-cache.json's
7-day window are flagged (stale=true in --json; "(stale)" in pretty output).

Use --json for skill consumption.`,
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

func runModels(vendor string, asJSON bool) error {
	cache, err := models.Load()
	if err != nil {
		return reportModelsErr(asJSON, vendor, codeModelsCacheLoadFailed,
			fmt.Errorf("load cache: %w", err),
			"check ~/.config/cc-fleet/models-cache.json")
	}

	vc, ok := cache.Vendors[vendor]
	if !ok {
		return reportModelsErr(asJSON, vendor, codeModelsVendorNotInCache,
			fmt.Errorf("vendor not in cache (run: cc-fleet refresh %s)", vendor),
			fmt.Sprintf("cc-fleet refresh %s", vendor))
	}

	stale := models.IsStale(vc)
	if asJSON {
		env := modelsEnvelope{
			OK:        true,
			Vendor:    vc.Vendor,
			Endpoint:  vc.Endpoint,
			FetchedAt: vc.FetchedAt,
			Stale:     stale,
			Models:    vc.Models,
		}
		if env.Models == nil {
			env.Models = []models.Model{}
		}
		data, mErr := json.Marshal(env)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "models: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return nil
	}

	// Pretty mode: header line + table.
	staleTag := ""
	if stale {
		staleTag = " (stale)"
	}
	fmt.Printf("vendor %s — %d model(s), fetched %s%s\n",
		vc.Vendor, len(vc.Models),
		vc.FetchedAt.Format(time.RFC3339), staleTag)

	if len(vc.Models) == 0 {
		fmt.Println("(no models — try cc-fleet refresh", vendor+")")
		return nil
	}

	// Stable, alphabetical order so successive runs diff cleanly.
	sorted := append([]models.Model(nil), vc.Models...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tOWNED_BY")
	for _, m := range sorted {
		fmt.Fprintf(w, "%s\t%s\n", m.ID, m.OwnedBy)
	}
	return w.Flush()
}

// reportModelsErr writes the failure envelope (JSON or pretty) and exits
// non-zero so the skill never sees a half-line. Returns nil only because
// the os.Exit call won't return — keeps the signature consistent with
// reportSpawn / reportTeardown.
func reportModelsErr(asJSON bool, vendor, code string, err error, suggestion string) error {
	if asJSON {
		env := modelsEnvelope{
			OK:         false,
			Vendor:     vendor,
			Stale:      false,
			Models:     []models.Model{},
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
