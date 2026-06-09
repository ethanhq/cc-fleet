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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/permmode"
)

// SchemaVersion is the only supported vendors.toml schema version.
const SchemaVersion = 1

// CodexOAuthBackend is the secret_backend value for a codex (ChatGPT
// subscription) provider. Its keyget hands back a loopback handshake secret —
// the upstream OAuth bearer lives only in the codex proxy daemon — so its
// base_url and models_endpoint MUST be loopback http (enforced at validate, so a
// hand-edited or `cc-fleet edit` vendor can never send that secret off-host).
const CodexOAuthBackend = "codex-oauth"

// validSecretBackends is the closed set of supported secret_backend values.
// Order is the canonical order used in error messages.
var validSecretBackends = []string{"file", "pass", "1password", "vault", "keyring", CodexOAuthBackend}

// Wire protocol values for Vendor.protocol — orthogonal to secret_backend. "" is
// Anthropic-native (the vendor speaks the Anthropic Messages API directly, no
// daemon). The others ride the loopback conversion daemon: openai-chat and
// openai-responses speak the OpenAI API with a real key; codex-oauth reuses a
// ChatGPT subscription over OAuth.
const (
	ProtocolOpenAIChat      = "openai-chat"
	ProtocolOpenAIResponses = "openai-responses"
	ProtocolCodexOAuth      = CodexOAuthBackend // "codex-oauth"
)

// validProtocols is the closed set of supported protocol values (the empty
// default included), rejected at Load like every other enum.
var validProtocols = []string{"", ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolCodexOAuth}

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
	// Protocol is the wire class (one of validProtocols). "" = Anthropic-native;
	// omitempty keeps every existing vendors.toml byte-identical. A codex-oauth
	// secret_backend with no protocol is treated as the codex protocol — see
	// EffectiveProtocol.
	Protocol string `toml:"protocol,omitempty"`
	// UpstreamURL is the real OpenAI-compatible base URL for an openai-* protocol
	// (claude talks to the loopback daemon on base_url; the daemon forwards here).
	// Required for openai-*, forbidden otherwise. May carry a clean path prefix,
	// usually ending in /v1.
	UpstreamURL string `toml:"upstream_url,omitempty"`
	// StrongModel / FastModel are the optional "strong" and "fast" capability
	// slots; blank → the slot follows DefaultModel. They populate the Claude Code
	// model-tier env in the provider profile (opus/haiku aliases + subagent), so a
	// teammate or subagent never falls back to a built-in claude-* id the provider
	// can't serve. omitempty keeps existing files byte-stable.
	StrongModel string `toml:"strong_model,omitempty"`
	FastModel   string `toml:"fast_model,omitempty"`
	// Effort is the optional reasoning-effort level (one of validEfforts; "" =
	// unset). It is written into the profile by the least-intrusive knob that can
	// express it — "max" via the CLAUDE_CODE_EFFORT_LEVEL env, the others via the
	// settings effortLevel field — so a session /effort can still override a
	// non-max default.
	Effort string `toml:"effort,omitempty"`
	// DefaultPermission is the permission mode cc-fleet run uses when the caller
	// passes neither --permission-mode nor --dangerously-skip-permissions ("" = no
	// default → Claude's own default mode). Run-only; spawn inherits the lead's
	// mode and subagent keeps its own default.
	DefaultPermission string `toml:"default_permission,omitempty"`
}

// The reserved capability keywords ResolveModel (and --model) accept in addition
// to a literal model id.
const (
	ModelSlotDefault = "default"
	ModelSlotStrong  = "strong"
	ModelSlotFast    = "fast"
)

// contextMarker1M is Claude Code's model-id suffix that displays a 1M context
// window; Claude Code strips it before the API request, so the provider never
// sees it. Carried inside the stored model id — see With1M / Strip1M.
const contextMarker1M = "[1m]"

// validEfforts is the closed set of reasoning-effort levels ("" = unset
// included), rejected at Load like every other enum.
var validEfforts = []string{"", "low", "medium", "high", "xhigh", "max"}

func isValidEffort(e string) bool {
	for _, ok := range validEfforts {
		if e == ok {
			return true
		}
	}
	return false
}

// EffortLevels returns the non-empty reasoning-effort levels in canonical order
// (the dropdown vocabulary; "" / unset is handled at the UI boundary). One source
// so the config validator and the TUI picker can't drift.
func EffortLevels() []string {
	out := make([]string, 0, len(validEfforts))
	for _, e := range validEfforts {
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

// StrongModelOrDefault / FastModelOrDefault resolve a capability slot to a
// concrete model id, falling back to DefaultModel when the slot is blank.
func (v *Vendor) StrongModelOrDefault() string {
	if v.StrongModel != "" {
		return v.StrongModel
	}
	return v.DefaultModel
}

func (v *Vendor) FastModelOrDefault() string {
	if v.FastModel != "" {
		return v.FastModel
	}
	return v.DefaultModel
}

// ResolveModel maps a requested model to a concrete vendor model id: the reserved
// keywords default/strong/fast name the matching slot, any other value is a
// literal id passed through, and "" is DefaultModel. Keyword-first — the three
// reserved words always name a slot (a real model id is never one of these bare
// words). One resolver for every launcher (spawn / subagent / run, and the
// workflow leaf via subagent.Run) so the keyword semantics can't drift.
func (v *Vendor) ResolveModel(requested string) string {
	switch requested {
	case "", ModelSlotDefault:
		return v.DefaultModel
	case ModelSlotStrong:
		return v.StrongModelOrDefault()
	case ModelSlotFast:
		return v.FastModelOrDefault()
	default:
		return requested
	}
}

// With1M appends the 1M-context marker to a model id, idempotently (never doubles
// it); a blank id is returned unchanged. Strip1M removes a TRAILING marker (an
// interior "[1m]" is left alone); Has1M reports a trailing marker (the TUI
// toggle's on/off state).
func With1M(id string) string {
	if id == "" || strings.HasSuffix(id, contextMarker1M) {
		return id
	}
	return id + contextMarker1M
}

func Strip1M(id string) string { return strings.TrimSuffix(id, contextMarker1M) }

func Has1M(id string) bool { return strings.HasSuffix(id, contextMarker1M) }

// EffectiveProtocol resolves the wire protocol, applying the backward-compat rule
// that a codex-oauth secret_backend with no explicit protocol means the codex
// protocol — the codex providers shipped before the protocol field existed.
func (v *Vendor) EffectiveProtocol() string {
	if v.Protocol == "" && v.SecretBackend == CodexOAuthBackend {
		return ProtocolCodexOAuth
	}
	return v.Protocol
}

// DaemonBacked reports whether the vendor's traffic rides the loopback conversion
// daemon (any non-Anthropic-native protocol).
func (v *Vendor) DaemonBacked() bool { return v.EffectiveProtocol() != "" }

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
	// Each codex provider owns a distinct login credential: the single cli-ride-capable
	// default (empty / the "codex-oauth" sentinel) plus one named credential per
	// secret_ref. Two codex rows resolving to the same credential would clobber one
	// login, so reject that here (the per-vendor pass cannot see across rows).
	seenCred := make(map[string]string, len(names))
	for _, name := range names {
		v := c.Vendors[name]
		if v.EffectiveProtocol() != ProtocolCodexOAuth {
			continue
		}
		cred := v.SecretRef
		if cred == "" || cred == CodexOAuthBackend {
			cred = CodexOAuthBackend // canonical default
		}
		if prev, dup := seenCred[cred]; dup {
			return fmt.Errorf("config: codex providers %q and %q share credential %q; give each a distinct secret_ref", prev, name, v.SecretRef)
		}
		seenCred[cred] = name
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
	if !isValidProtocol(v.Protocol) {
		return fmt.Errorf("config: vendor %q: protocol %q invalid (want one of %v)",
			name, v.Protocol, validProtocols)
	}
	if err := v.validateWire(name); err != nil {
		return err
	}
	if !isValidKeyRotation(v.KeyRotation) {
		return fmt.Errorf("config: vendor %q: key_rotation %q invalid (want one of %v)",
			name, v.KeyRotation, ValidKeyRotations())
	}
	if !isValidEffort(v.Effort) {
		return fmt.Errorf("config: vendor %q: effort %q invalid (want one of %v)",
			name, v.Effort, validEfforts)
	}
	if v.DefaultPermission != "" && !permmode.IsValid(v.DefaultPermission) {
		return fmt.Errorf("config: vendor %q: default_permission %q invalid (want one of %v)",
			name, v.DefaultPermission, permmode.Modes)
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

func isValidProtocol(p string) bool {
	for _, ok := range validProtocols {
		if p == ok {
			return true
		}
	}
	return false
}

// validateWire enforces the protocol/secret_backend/upstream_url/loopback
// cross-checks for the resolved (compat-normalized) wire protocol.
func (v *Vendor) validateWire(name string) error {
	switch v.EffectiveProtocol() {
	case ProtocolCodexOAuth:
		if v.SecretBackend != CodexOAuthBackend {
			return fmt.Errorf("config: vendor %q: codex-oauth protocol requires secret_backend %q", name, CodexOAuthBackend)
		}
		// secret_ref is the per-provider credential id; it becomes a token-file and
		// flock name component, so it must be a path-safe identifier (the legacy
		// "codex-oauth" sentinel and any provider-name-derived ref both qualify).
		if err := ids.ValidateVendorName(v.SecretRef); err != nil {
			return fmt.Errorf("config: vendor %q: codex secret_ref %w", name, err)
		}
		if v.UpstreamURL != "" {
			return fmt.Errorf("config: vendor %q: codex-oauth must not set upstream_url", name)
		}
		// keyget hands the launched claude only a handshake secret it presents on
		// base_url, and a probe sends it to models_endpoint — both must be loopback
		// http so that secret can never leave the host.
		if _, err := ParseLoopbackURL(v.BaseURL); err != nil {
			return fmt.Errorf("config: vendor %q: codex base_url %w", name, err)
		}
		if _, err := ParseLoopbackURL(v.ModelsEndpoint); err != nil {
			return fmt.Errorf("config: vendor %q: codex models_endpoint %w", name, err)
		}
	case ProtocolOpenAIChat, ProtocolOpenAIResponses:
		if v.SecretBackend == CodexOAuthBackend {
			return fmt.Errorf("config: vendor %q: %s protocol carries a real key, not the codex-oauth backend", name, v.EffectiveProtocol())
		}
		if v.UpstreamURL == "" {
			return fmt.Errorf("config: vendor %q: %s protocol requires upstream_url", name, v.EffectiveProtocol())
		}
		if err := ValidateUpstreamURL(v.UpstreamURL); err != nil {
			return fmt.Errorf("config: vendor %q: upstream_url %w", name, err)
		}
		// claude talks to the loopback conversion daemon on base_url; the real
		// upstream lives in upstream_url.
		if _, err := ParseLoopbackURL(v.BaseURL); err != nil {
			return fmt.Errorf("config: vendor %q: openai base_url %w", name, err)
		}
	default: // "" Anthropic-native — claude talks to the vendor directly.
		if v.UpstreamURL != "" {
			return fmt.Errorf("config: vendor %q: upstream_url is only valid for an openai-* protocol", name)
		}
	}
	return nil
}

// ValidateUpstreamURL validates an openai-* upstream_url: an OpenAI-compatible
// base URL (scheme://host[:port][/clean/prefix], the prefix usually ending in
// /v1) onto which the daemon joins endpoint suffixes with url.JoinPath. https is
// required for a remote host; http is allowed only for a loopback host (local
// Ollama/vLLM). userinfo, query, fragment, and unclean or .. path segments are
// rejected — it rides the daemon argv.
func ValidateUpstreamURL(raw string) error {
	// Validate the exact stored value (the daemon uses it verbatim): surrounding
	// whitespace would pass a trimmed check yet break url.JoinPath at run time.
	if raw != strings.TrimSpace(raw) {
		return fmt.Errorf("%q must not have surrounding whitespace", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%q missing host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("%q must not carry userinfo", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not carry a query or fragment", raw)
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		// path.Clean rejects every real traversal (".."/"."/"//"); an explicit
		// Contains("..") would also reject a legitimate segment that merely contains
		// the bytes, so it is not needed.
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
			return fmt.Errorf("%q has an unsafe path %q", raw, u.Path)
		}
	}
	loopback := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost"
	switch u.Scheme {
	case "https":
		// ok for any host
	case "http":
		if !loopback {
			return fmt.Errorf("%q must use https for a remote host (http is loopback-only)", raw)
		}
	default:
		return fmt.Errorf("%q must use http or https (got %q)", raw, u.Scheme)
	}
	return nil
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

// ParseLoopbackURL validates raw is a plain http://127.0.0.1|localhost[:port] URL
// (no userinfo, no https / remote host) and returns it. The one definition the
// codex-oauth validate path and the codex proxy daemon both use, so the loopback
// invariant can't drift between load-time rejection and run-time defense.
func ParseLoopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("%q must use http (loopback only)", raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%q must not carry userinfo", raw)
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return nil, fmt.Errorf("%q must be loopback (127.0.0.1 or localhost), got %q", raw, h)
	}
	return u, nil
}
