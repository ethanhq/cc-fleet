package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vendor returns a minimal valid Vendor for the resolver tests.
func vendor(name string, enabled bool) *Vendor {
	return &Vendor{
		Name:           name,
		BaseURL:        "https://api." + name + ".com/anthropic",
		DefaultModel:   name + "-model",
		ModelsEndpoint: "https://api." + name + ".com/v1/models",
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        enabled,
	}
}

func cfgWith(def string, vs ...*Vendor) *Config {
	c := &Config{Version: SchemaVersion, Vendors: map[string]*Vendor{}, DefaultProvider: def}
	for _, v := range vs {
		c.Vendors[v.Name] = v
	}
	return c
}

// TestResolveProvider walks the precedence ladder: explicit > default > sole > error,
// and the two no-fall-through error cases (disabled / unknown default).
func TestResolveProvider(t *testing.T) {
	cases := []struct {
		name       string
		cfg        *Config
		requested  string
		wantName   string
		wantSource string
		wantErr    error
	}{
		{"explicit wins", cfgWith("glm", vendor("glm", true), vendor("kimi", true)), "kimi", "kimi", "explicit", nil},
		{"explicit even when disabled", cfgWith("", vendor("kimi", false)), "kimi", "kimi", "explicit", nil},
		{"configured default", cfgWith("glm", vendor("glm", true), vendor("kimi", true)), "", "glm", "default", nil},
		{"sole enabled auto", cfgWith("", vendor("kimi", true)), "", "kimi", "sole", nil},
		{"sole among disabled", cfgWith("", vendor("kimi", true), vendor("glm", false)), "", "kimi", "sole", nil},
		{"none enabled", cfgWith("", vendor("a", false), vendor("b", false)), "", "", "", ErrNoDefaultProvider},
		{"multiple, no default", cfgWith("", vendor("a", true), vendor("b", true)), "", "", "", ErrNoDefaultProvider},
		{"default disabled never falls through", cfgWith("glm", vendor("glm", false), vendor("kimi", true)), "", "", "", ErrDefaultProviderDisabled},
		{"default unknown", cfgWith("gone", vendor("kimi", true)), "", "", "", ErrDefaultProviderUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, source, err := tc.cfg.ResolveProvider(tc.requested)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || source != tc.wantSource {
				t.Fatalf("got (%q,%q), want (%q,%q)", name, source, tc.wantName, tc.wantSource)
			}
		})
	}
}

// TestDefaultProviderRoundTrips: a config saved with default_provider reloads with
// it, and a config WITHOUT the key (incl. one with a provider literally named
// "default") loads with DefaultProvider "" and is not rejected.
func TestDefaultProviderRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")

	c := cfgWith("glm", vendor("glm", true), vendor("kimi", true))
	if err := SaveToPath(c, path); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `default_provider = "glm"`) {
		t.Fatalf("saved file missing default_provider line:\n%s", data)
	}
	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.DefaultProvider != "glm" {
		t.Fatalf("reloaded default = %q, want glm", got.DefaultProvider)
	}

	// A provider literally named "default" is NOT reserved — it round-trips.
	c2 := cfgWith("", vendor("default", true))
	if err := SaveToPath(c2, path); err != nil {
		t.Fatalf("save provider named default: %v", err)
	}
	got2, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("load provider named default: %v", err)
	}
	if _, ok := got2.Vendors["default"]; !ok {
		t.Fatalf("provider %q lost on round-trip", "default")
	}
	if got2.DefaultProvider != "" {
		t.Fatalf("default = %q, want empty (no key written)", got2.DefaultProvider)
	}
}

// TestParseDefaultProviderScalarWrongType: a non-string, non-table default_provider
// (e.g. an integer) is not the scalar key and not a vendor table → rejected.
func TestParseDefaultProviderScalarWrongType(t *testing.T) {
	_, err := parse([]byte("version = 1\ndefault_provider = 5\n"))
	if err == nil || !strings.Contains(err.Error(), "default_provider") {
		t.Fatalf("err = %v, want a default_provider error", err)
	}
}

// TestParseLegacyDefaultProviderTable: a config with a vendor TABLE named
// "default_provider" (a hand-named provider, not the scalar key) must still load —
// it parses as a vendor — so an old config never bricks.
func TestParseLegacyDefaultProviderTable(t *testing.T) {
	body := `version = 1

[default_provider]
base_url = "https://api.x.com/anthropic"
default_model = "x-model"
models_endpoint = "https://api.x.com/v1/models"
secret_backend = "file"
secret_ref = "x.key"
enabled = true
`
	cfg, err := parse([]byte(body))
	if err != nil {
		t.Fatalf("parse legacy table: %v", err)
	}
	if _, ok := cfg.Vendors["default_provider"]; !ok {
		t.Fatalf("vendor named default_provider lost (vendors: %v)", cfg.Vendors)
	}
	if cfg.DefaultProvider != "" {
		t.Fatalf("DefaultProvider = %q, want empty (a table is not the scalar)", cfg.DefaultProvider)
	}
}
