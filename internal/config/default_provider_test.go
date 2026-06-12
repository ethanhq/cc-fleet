package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// provider returns a minimal valid Provider for the resolver tests.
func provider(name string, enabled bool) *Provider {
	return &Provider{
		Name:           name,
		BaseURL:        "https://api." + name + ".com/anthropic",
		DefaultModel:   name + "-model",
		ModelsEndpoint: "https://api." + name + ".com/v1/models",
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        enabled,
	}
}

func cfgWith(def string, vs ...*Provider) *Config {
	c := &Config{Version: SchemaVersion, Providers: map[string]*Provider{}, DefaultProvider: def}
	for _, v := range vs {
		c.Providers[v.Name] = v
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
		{"explicit wins", cfgWith("glm", provider("glm", true), provider("kimi", true)), "kimi", "kimi", "explicit", nil},
		{"explicit even when disabled", cfgWith("", provider("kimi", false)), "kimi", "kimi", "explicit", nil},
		{"configured default", cfgWith("glm", provider("glm", true), provider("kimi", true)), "", "glm", "default", nil},
		{"sole enabled auto", cfgWith("", provider("kimi", true)), "", "kimi", "sole", nil},
		{"sole among disabled", cfgWith("", provider("kimi", true), provider("glm", false)), "", "kimi", "sole", nil},
		{"none enabled", cfgWith("", provider("a", false), provider("b", false)), "", "", "", ErrNoDefaultProvider},
		{"multiple, no default", cfgWith("", provider("a", true), provider("b", true)), "", "", "", ErrNoDefaultProvider},
		{"default disabled never falls through", cfgWith("glm", provider("glm", false), provider("kimi", true)), "", "", "", ErrDefaultProviderDisabled},
		{"default unknown", cfgWith("gone", provider("kimi", true)), "", "", "", ErrDefaultProviderUnknown},
		{"reserved native default rejected", cfgWith(ReservedNativeProvider, provider(ReservedNativeProvider, true)), "", "", "", ErrDefaultProviderReserved},
		{"sole enabled reserved row never auto-resolves", cfgWith("", provider(ReservedNativeProvider, true)), "", "", "", ErrNoDefaultProvider},
		{"explicit reserved passes through", cfgWith(""), ReservedNativeProvider, ReservedNativeProvider, "explicit", nil},
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
	path := filepath.Join(dir, "providers.toml")

	c := cfgWith("glm", provider("glm", true), provider("kimi", true))
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
	c2 := cfgWith("", provider("default", true))
	if err := SaveToPath(c2, path); err != nil {
		t.Fatalf("save provider named default: %v", err)
	}
	got2, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("load provider named default: %v", err)
	}
	if _, ok := got2.Providers["default"]; !ok {
		t.Fatalf("provider %q lost on round-trip", "default")
	}
	if got2.DefaultProvider != "" {
		t.Fatalf("default = %q, want empty (no key written)", got2.DefaultProvider)
	}
}

// TestParseDefaultProviderScalarWrongType: a non-string, non-table default_provider
// (e.g. an integer) is not the scalar key and not a provider table → rejected.
func TestParseDefaultProviderScalarWrongType(t *testing.T) {
	_, err := parse([]byte("version = 1\ndefault_provider = 5\n"))
	if err == nil || !strings.Contains(err.Error(), "default_provider") {
		t.Fatalf("err = %v, want a default_provider error", err)
	}
}

// TestParseLegacyDefaultProviderTable: a config with a provider TABLE named
// "default_provider" (a hand-named provider, not the scalar key) must still load —
// it parses as a provider — so an old config never bricks.
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
	if _, ok := cfg.Providers["default_provider"]; !ok {
		t.Fatalf("provider named default_provider lost (providers: %v)", cfg.Providers)
	}
	if cfg.DefaultProvider != "" {
		t.Fatalf("DefaultProvider = %q, want empty (a table is not the scalar)", cfg.DefaultProvider)
	}
}
