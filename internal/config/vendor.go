// Package config reads, writes, and validates ~/.config/cc-fleet/vendors.toml.
//
// vendors.toml is the single source of truth users edit by hand. This package
// only deals with the on-disk format and schema validation — it does not know
// about secrets backends, profile generation, or spawn fingerprints.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// SchemaVersion is the only supported vendors.toml schema version.
const SchemaVersion = 1

// validSecretBackends is the closed set of supported secret_backend values.
// Order is the canonical order used in error messages.
var validSecretBackends = []string{"file", "pass", "1password", "vault", "keyring"}

// Config is the parsed contents of vendors.toml.
//
// Vendors is keyed by vendor name (the TOML table header, e.g. "deepseek").
// Each *Vendor.Name mirrors that key for convenience.
type Config struct {
	Version int                `toml:"version"`
	Vendors map[string]*Vendor `toml:"-"`
}

// Vendor is one [vendor] table inside vendors.toml.
//
// Field names and TOML tags are part of the public schema — do not rename
// without bumping SchemaVersion.
type Vendor struct {
	Name           string    `toml:"-"`
	BaseURL        string    `toml:"base_url"`
	DefaultModel   string    `toml:"default_model"`
	ModelsEndpoint string    `toml:"models_endpoint"`
	SecretBackend  string    `toml:"secret_backend"`
	SecretRef      string    `toml:"secret_ref"`
	Enabled        bool      `toml:"enabled"`
	AddedAt        time.Time `toml:"added_at"`
	// KeyRotation selects the per-worker file-backend multi-key rotation
	// strategy: "" (= "off") | "off" | "round_robin" | "random". omitempty keeps
	// off/single-key vendors' files byte-identical (no key_rotation line); an
	// absent field parses as off.
	KeyRotation string `toml:"key_rotation,omitempty"`
}

// Load reads, parses, and returns the contents of the default vendors.toml.
//
// A missing file is NOT an error: an empty Config (version=1, no vendors) is
// returned so first-run callers can Save() it back without special-casing.
func Load() (*Config, error) {
	path, err := VendorsPath()
	if err != nil {
		return nil, err
	}
	return LoadFromPath(path)
}

// LoadFromPath is Load() against an explicit path. Used by tests.
//
// Default-strict: it validates after parse, so an invalid key_rotation /
// secret_backend / version is rejected here rather than silently absorbed by a
// downstream enum default. Load is a runtime path, so refusing beats guessing.
func LoadFromPath(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{Version: SchemaVersion, Vendors: map[string]*Vendor{}}, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := parse(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validate %s: %w", path, err)
	}
	return cfg, nil
}

// parse decodes raw TOML bytes into a Config. Each non-"version" top-level
// table is treated as a vendor; we unmarshal it into a *Vendor and stamp its
// Name from the table key.
func parse(data []byte) (*Config, error) {
	// Decode into a generic map so we can discover vendor table names without
	// hard-coding them.
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("config: parse toml: %w", err)
	}

	cfg := &Config{Version: 0, Vendors: map[string]*Vendor{}}

	if v, ok := raw["version"]; ok {
		// TOML integers decode as int64.
		switch n := v.(type) {
		case int64:
			cfg.Version = int(n)
		case int:
			cfg.Version = n
		default:
			return nil, fmt.Errorf("config: version field has wrong type %T (want integer)", v)
		}
	}

	// Re-decode each vendor table individually into a typed *Vendor. We use
	// toml.Marshal on the sub-map and Unmarshal into the struct so we get the
	// standard struct-tag handling (including time.Time parsing).
	for key, val := range raw {
		if key == "version" {
			continue
		}
		sub, ok := val.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config: top-level key %q is not a table", key)
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(sub); err != nil {
			return nil, fmt.Errorf("config: re-encode vendor %q: %w", key, err)
		}
		v := &Vendor{}
		if _, err := toml.Decode(buf.String(), v); err != nil {
			return nil, fmt.Errorf("config: decode vendor %q: %w", key, err)
		}
		v.Name = key
		cfg.Vendors[key] = v
	}

	return cfg, nil
}

// Save writes c to the default vendors.toml path atomically with mode 0600.
func Save(c *Config) error {
	path, err := VendorsPath()
	if err != nil {
		return err
	}
	return SaveToPath(c, path)
}

// SaveToPath writes c to path atomically (write to *.tmp + rename) with mode
// 0600. Validation runs first; an invalid Config is never persisted.
func SaveToPath(c *Config, path string) error {
	if err := c.Validate(); err != nil {
		return err
	}

	// Build TOML body. We hand-write the top-level structure (version + one
	// table per vendor in sorted order) so the file is stable and diff-friendly.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "version = %d\n", c.Version)

	names := make([]string, 0, len(c.Vendors))
	for name := range c.Vendors {
		names = append(names, name)
	}
	sort.Strings(names)

	enc := toml.NewEncoder(&buf)
	for _, name := range names {
		v := c.Vendors[name]
		buf.WriteString("\n[")
		buf.WriteString(name)
		buf.WriteString("]\n")
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("config: encode vendor %q: %w", name, err)
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}

	if err := fileutil.AtomicWrite(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// Validate enforces the schema. It returns nil iff Save would produce a
// well-formed vendors.toml.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil Config")
	}
	if c.Version != SchemaVersion {
		return fmt.Errorf("config: unsupported version %d (want %d)", c.Version, SchemaVersion)
	}
	// Stable iteration order for predictable error messages in tests.
	names := make([]string, 0, len(c.Vendors))
	for name := range c.Vendors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := c.Vendors[name]
		if v == nil {
			return fmt.Errorf("config: vendor %q is nil", name)
		}
		if err := v.validate(name); err != nil {
			return err
		}
	}
	return nil
}

// validate checks one Vendor. mapKey is the vendors.toml table name; we treat
// it as authoritative when v.Name is empty (e.g. freshly Loaded).
func (v *Vendor) validate(mapKey string) error {
	name := v.Name
	if name == "" {
		name = mapKey
	}
	if name == "" {
		return errors.New("config: vendor has empty name")
	}
	// Reject a hand-edited table name whose grammar isn't path/shell-safe: it
	// becomes v.Name and flows into profile.ProfilePath (filepath.Join →
	// traversal) and the apiKeyHelper "<bin> keyget <name>" (shell-evaluated →
	// injection). Validating at Load stops it first. Grammar lives in internal/ids
	// to avoid a userops import cycle.
	if err := ids.ValidateVendorName(name); err != nil {
		return fmt.Errorf("config: vendor %q: %w", name, err)
	}
	if v.BaseURL == "" {
		return fmt.Errorf("config: vendor %q: base_url is required", name)
	}
	if err := validateHTTPURL("base_url", v.BaseURL); err != nil {
		return fmt.Errorf("config: vendor %q: %w", name, err)
	}
	if v.DefaultModel == "" {
		return fmt.Errorf("config: vendor %q: default_model is required", name)
	}
	if v.ModelsEndpoint == "" {
		return fmt.Errorf("config: vendor %q: models_endpoint is required", name)
	}
	if err := validateHTTPURL("models_endpoint", v.ModelsEndpoint); err != nil {
		return fmt.Errorf("config: vendor %q: %w", name, err)
	}
	if v.SecretBackend == "" {
		return fmt.Errorf("config: vendor %q: secret_backend is required", name)
	}
	if !isValidSecretBackend(v.SecretBackend) {
		return fmt.Errorf("config: vendor %q: secret_backend %q invalid (want one of %v)",
			name, v.SecretBackend, validSecretBackends)
	}
	if v.SecretRef == "" {
		return fmt.Errorf("config: vendor %q: secret_ref is required", name)
	}
	if !isValidKeyRotation(v.KeyRotation) {
		return fmt.Errorf("config: vendor %q: key_rotation %q invalid (want one of %v)",
			name, v.KeyRotation, ValidKeyRotations())
	}
	return nil
}

func isValidSecretBackend(b string) bool {
	for _, ok := range validSecretBackends {
		if b == ok {
			return true
		}
	}
	return false
}

func isValidKeyRotation(r string) bool {
	for _, ok := range ValidKeyRotations() {
		if r == ok {
			return true
		}
	}
	return false
}

// validateHTTPURL ensures s is a syntactically valid absolute http(s) URL.
func validateHTTPURL(field, s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s %q is not a valid URL: %w", field, s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s %q must use http or https scheme (got %q)", field, s, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q missing host", field, s)
	}
	return nil
}
