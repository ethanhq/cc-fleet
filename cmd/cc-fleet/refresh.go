package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/neterr"
	"github.com/ethanhq/cc-fleet/internal/redact"
)

// refreshSuccess is the JSON shape emitted on success.
type refreshSuccess struct {
	OK         bool      `json:"ok"`
	Vendor     string    `json:"vendor"`
	ModelCount int       `json:"model_count"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// refreshError is the JSON shape emitted on failure. Stable error_code values
// let the skill dispatch without parsing prose.
type refreshError struct {
	OK        bool   `json:"ok"`
	Vendor    string `json:"vendor"`
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

// Error codes for `cc-fleet refresh <vendor>` — kept stable for skill dispatch.
const (
	codeRefreshVendorUnknown     = "VENDOR_UNKNOWN"
	codeRefreshConfigLoadFailed  = "CONFIG_LOAD_FAILED"
	codeRefreshKeyInvalid        = "KEY_INVALID"
	codeRefreshVendorUnreachable = "VENDOR_UNREACHABLE"
	codeRefreshFailed            = "REFRESH_FAILED"
	codeRefreshSaveFailed        = "SAVE_FAILED"
)

// refreshTimeout caps the whole refresh including HTTP. We use ctx-level
// deadline so a hung vendor surfaces as VENDOR_UNREACHABLE rather than
// dragging the cmd into an unbounded wait.
const refreshTimeout = 15 * time.Second

func newRefreshCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "refresh <vendor>",
		Short: "Re-query a vendor's /v1/models endpoint and update the cache",
		Long: `Force-refresh the local model cache for <vendor>.

Looks up the vendor in vendors.toml, calls its models_endpoint with the
configured secret_backend's API key, and updates
~/.config/cc-fleet/models-cache.json. Use --json for skill consumption.

Errors map to these error codes:
  KEY_INVALID         vendor returned HTTP 401
  VENDOR_UNREACHABLE  DNS / connect / HTTP timeout
  REFRESH_FAILED      anything else (parse, non-401 4xx/5xx)`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			return runRefresh(args[0], asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

func runRefresh(vendor string, asJSON bool) error {
	cfg, err := config.Load()
	if err != nil {
		return reportRefreshErr(asJSON, vendor, codeRefreshConfigLoadFailed,
			fmt.Errorf("load vendors.toml: %w", err))
	}
	v, ok := cfg.Vendors[vendor]
	if !ok {
		return reportRefreshErr(asJSON, vendor, codeRefreshVendorUnknown,
			fmt.Errorf("vendor %q not in vendors.toml", vendor))
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()

	fetched, err := models.Fetch(ctx, v)
	if err != nil {
		return reportRefreshErr(asJSON, vendor, classifyFetchErr(err), err)
	}

	cache, err := models.Load()
	if err != nil {
		return reportRefreshErr(asJSON, vendor, codeRefreshSaveFailed,
			fmt.Errorf("load cache: %w", err))
	}
	if cache.Vendors == nil {
		cache.Vendors = map[string]*models.VendorCache{}
	}
	now := time.Now().UTC()
	cache.Vendors[vendor] = &models.VendorCache{
		Vendor:    vendor,
		Endpoint:  v.ModelsEndpoint,
		FetchedAt: now,
		Models:    fetched,
	}
	if err := models.Save(cache); err != nil {
		return reportRefreshErr(asJSON, vendor, codeRefreshSaveFailed,
			fmt.Errorf("save cache: %w", err))
	}

	return reportRefreshOK(asJSON, vendor, len(fetched), now)
}

// classifyFetchErr maps a models.Fetch error onto one of the refresh error
// codes. Order matters: KEY_INVALID is the sentinel and beats any
// transport-level classification.
func classifyFetchErr(err error) string {
	if errors.Is(err, models.ErrKeyInvalid) {
		return codeRefreshKeyInvalid
	}
	if neterr.IsTransport(err) {
		return codeRefreshVendorUnreachable
	}
	return codeRefreshFailed
}

func reportRefreshOK(asJSON bool, vendor string, count int, fetchedAt time.Time) error {
	if asJSON {
		out := refreshSuccess{
			OK:         true,
			Vendor:     vendor,
			ModelCount: count,
			FetchedAt:  fetchedAt,
		}
		data, mErr := json.Marshal(out)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "refresh: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("refreshed %s: %d model(s) at %s\n",
		vendor, count, fetchedAt.Format(time.RFC3339))
	return nil
}

func reportRefreshErr(asJSON bool, vendor, code string, err error) error {
	// Defense-in-depth: pipe the final error string through redact.MaskKeyLike
	// before it reaches the JSON envelope or stderr, so any future code path that
	// accidentally includes a key fragment is still scrubbed at the surface.
	safeMsg := redact.MaskKeyLikeString(err.Error())
	if asJSON {
		out := refreshError{
			OK:        false,
			Vendor:    vendor,
			Error:     safeMsg,
			ErrorCode: code,
		}
		data, mErr := json.Marshal(out)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "refresh: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "refresh: %s: %s\n", code, safeMsg)
	os.Exit(1)
	return nil
}
