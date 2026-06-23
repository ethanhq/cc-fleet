package config

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/BurntSushi/toml"
)

// BundleVersion is the export/import bundle format version. It is DELIBERATELY
// separate from SchemaVersion (the providers.toml schema): the bundle is a portable,
// keyless subset, so its format can evolve independently of the on-disk config.
const BundleVersion = 1

// Bundle is a portable, keyless provider roster: provider config rows plus the global
// default, carrying NO key material. It serializes like providers.toml but keyed by
// `bundle_version`. A daemon-backed (OpenAI-protocol) row's loopback base_url is left
// empty here — it is re-derived on the target machine at import time.
type Bundle struct {
	Version         int
	DefaultProvider string
	Providers       map[string]*Provider
}

// reservedBundleProviderName reports whether name collides with a top-level bundle key, so
// a provider table named it would clash with the scalar and the bundle would not parse
// back. Enforced on both marshal and parse so the two stay symmetric.
func reservedBundleProviderName(name string) bool {
	return name == "bundle_version" || name == "default_provider"
}

// MarshalBundle serializes b as TOML: `bundle_version`, an optional `default_provider`,
// and one [name] table per provider in sorted order. A provider name that collides with a
// reserved top-level key is rejected.
func MarshalBundle(b *Bundle) ([]byte, error) {
	for name := range b.Providers {
		if reservedBundleProviderName(name) {
			return nil, fmt.Errorf("config: %q cannot be a provider name in a bundle (reserved key)", name)
		}
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "bundle_version = %d\n", b.Version)
	if b.DefaultProvider != "" {
		fmt.Fprintf(&buf, "default_provider = %q\n", b.DefaultProvider)
	}
	names := make([]string, 0, len(b.Providers))
	for name := range b.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	enc := toml.NewEncoder(&buf)
	for _, name := range names {
		buf.WriteString("\n[")
		buf.WriteString(name)
		buf.WriteString("]\n")
		if err := enc.Encode(b.Providers[name]); err != nil {
			return nil, fmt.Errorf("config: encode bundle provider %q: %w", name, err)
		}
	}
	return buf.Bytes(), nil
}

// ParseBundle decodes a bundle's TOML and requires a recognized bundle_version. It does
// NOT validate provider rows — the import path merges them into a Config and runs
// Config.Validate(), the single strict gate.
func ParseBundle(data []byte) (*Bundle, error) {
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("config: parse bundle toml: %w", err)
	}
	v, ok := raw["bundle_version"]
	if !ok {
		return nil, fmt.Errorf("config: not a cc-fleet provider bundle (missing bundle_version)")
	}
	b := &Bundle{Providers: map[string]*Provider{}}
	switch n := v.(type) {
	case int64:
		b.Version = int(n)
	case int:
		b.Version = n
	default:
		return nil, fmt.Errorf("config: bundle_version has wrong type %T (want integer)", v)
	}
	if b.Version != BundleVersion {
		return nil, fmt.Errorf("config: unsupported bundle_version %d (this build understands %d)", b.Version, BundleVersion)
	}

	defaultIsScalar := false
	if dv, ok := raw["default_provider"]; ok {
		if s, isStr := dv.(string); isStr {
			b.DefaultProvider = s
			defaultIsScalar = true
		}
	}
	for key, val := range raw {
		if key == "bundle_version" {
			continue
		}
		if key == "default_provider" && defaultIsScalar {
			continue
		}
		if reservedBundleProviderName(key) {
			return nil, fmt.Errorf("config: %q cannot be a provider name in a bundle (reserved key)", key)
		}
		sub, ok := val.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("config: bundle key %q is not a table", key)
		}
		var buf bytes.Buffer
		if err := toml.NewEncoder(&buf).Encode(sub); err != nil {
			return nil, fmt.Errorf("config: re-encode bundle provider %q: %w", key, err)
		}
		p := &Provider{}
		if _, err := toml.Decode(buf.String(), p); err != nil {
			return nil, fmt.Errorf("config: decode bundle provider %q: %w", key, err)
		}
		p.Name = key
		b.Providers[key] = p
	}
	return b, nil
}
