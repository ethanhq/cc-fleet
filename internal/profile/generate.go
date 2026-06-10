package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// profileFile is the on-disk shape of a Claude Code settings/profile file.
//
// A named struct (rather than a map) makes MarshalIndent emit a stable key
// order — apiKeyHelper first, then env, then effortLevel (omitted when unset).
type profileFile struct {
	APIKeyHelper string            `json:"apiKeyHelper"`
	Env          map[string]string `json:"env"`
	// EffortLevel is the settings reasoning-effort field for a non-"max" provider
	// effort (max rides the env instead — see GenerateForProvider). omitempty so a
	// provider with no/effort=="max" emits no effortLevel key.
	EffortLevel string `json:"effortLevel,omitempty"`
}

// GenerateForProvider renders the profile JSON for v.
//
// helperBinary is the absolute path that will be written into the
// apiKeyHelper field; callers normally pass os.Executable() so the helper
// invocation stays bound to the same cc-fleet binary that wrote the profile.
// An empty helperBinary is an error here — WriteForProvider resolves
// os.Executable() before delegating.
//
// The output is 2-space indented and has no trailing newline.
func GenerateForProvider(v *config.Provider, helperBinary string) ([]byte, error) {
	if v == nil {
		return nil, errors.New("profile: nil provider")
	}
	if v.Name == "" {
		return nil, errors.New("profile: provider name is empty")
	}
	// Defense-in-depth: v.Name is concatenated into the apiKeyHelper command
	// ("<bin> keyget <name>") Claude Code hands to a shell. Re-validate the
	// grammar here so a malformed name can't be injected even if it bypassed
	// the config Load gate.
	if err := ids.ValidateProviderName(v.Name); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	if v.BaseURL == "" {
		return nil, fmt.Errorf("profile: provider %q: base_url is empty", v.Name)
	}
	if helperBinary == "" {
		return nil, errors.New("profile: helperBinary is empty")
	}
	if !filepath.IsAbs(helperBinary) {
		return nil, fmt.Errorf("profile: helperBinary %q must be absolute", helperBinary)
	}

	// Claude Code hands the apiKeyHelper string to a shell, so the install path
	// and provider arg must be POSIX-shell-quoted — a path with a space or
	// metacharacter would otherwise be word-split or interpreted. The provider KEY
	// never enters this string (keyget resolves it at runtime), so quoting
	// protects only the path + name. A metachar-free path is emitted verbatim so
	// the profile stays byte-stable.
	// Pin every Claude Code model slot to a provider model so a teammate/subagent
	// never falls back to a built-in claude-* id the provider can't serve — the
	// haiku slot drives background work (titles, context compaction, quick
	// classification), so leaving it unset breaks long sessions against a provider
	// base_url. The main model is the --model flag; these cover the opus/sonnet/
	// haiku aliases and the Task-subagent model. The [1m] context marker is
	// stripped here: only the main model (via --model) carries it, where Claude
	// Code's strip-before-request is the documented behavior.
	env := map[string]string{
		"ANTHROPIC_BASE_URL":             v.BaseURL,
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   config.Strip1M(v.StrongModelOrDefault()),
		"ANTHROPIC_DEFAULT_SONNET_MODEL": config.Strip1M(v.DefaultModel),
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  config.Strip1M(v.FastModelOrDefault()),
		"CLAUDE_CODE_SUBAGENT_MODEL":     config.Strip1M(v.DefaultModel),
	}
	pf := profileFile{
		APIKeyHelper: posixQuote(helperBinary) + " keyget " + posixQuote(v.Name),
		Env:          env,
	}
	// Reasoning effort: "max" only via the env (the settings effortLevel field
	// can't express it); the other levels via the lower-precedence effortLevel, so
	// a session /effort can still override the default. Empty → neither.
	switch v.Effort {
	case "":
	case "max":
		env["CLAUDE_CODE_EFFORT_LEVEL"] = "max"
	default:
		pf.EffortLevel = v.Effort
	}

	out, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("profile: marshal provider %q: %w", v.Name, err)
	}
	return out, nil
}

// posixQuote returns s safely embeddable in a /bin/sh command line: a string of
// only POSIX-safe characters is returned verbatim, anything else is single-quoted
// with embedded single quotes escaped. Kept local so the low-level profile
// package takes no dependency edge on the tmux package for one quoter.
func posixQuote(s string) string {
	if s != "" && isShellSafe(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' {
			// Close the quoted run, emit an escaped quote, reopen quoting.
			b.WriteString(`'\''`)
			continue
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// isShellSafe reports whether s consists solely of characters that need no
// shell quoting (a conservative allow-list — alphanumerics plus the handful of
// path-safe punctuation a real install path uses). Anything outside that set
// (space, $, (, ), ;, &, |, quotes, …) forces posixQuote to wrap s.
func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// WriteForProvider writes the profile for v to ProfilePath(v.Name) atomically
// with mode 0600. The parent directory is created on demand at mode 0700.
//
// If helperBinary is empty, os.Executable() is used so the apiKeyHelper field
// always carries an absolute path to the running binary.
//
// Returns the resolved profile path on success.
func WriteForProvider(v *config.Provider, helperBinary string) (string, error) {
	if v == nil {
		return "", errors.New("profile: nil provider")
	}
	if helperBinary == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("profile: resolve os.Executable: %w", err)
		}
		helperBinary = exe
	}

	data, err := GenerateForProvider(v, helperBinary)
	if err != nil {
		return "", err
	}

	path, err := ProfilePath(v.Name)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("profile: mkdir %s: %w", dir, err)
	}

	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return "", fmt.Errorf("profile: write %s: %w", path, err)
	}
	return path, nil
}

// RemoveForProvider deletes the profile file for provider.
//
// A missing file is NOT an error — this is intended for idempotent cleanup
// from teardown / uninstall paths.
func RemoveForProvider(provider string) error {
	path, err := ProfilePath(provider)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("profile: remove %s: %w", path, err)
	}
	return nil
}
