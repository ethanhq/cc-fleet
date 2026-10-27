package profile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

const (
	codexHomeDirName = "codex-home"
	codexProviderID  = "ccfleet"
	// keyget may shell out to pass / op / vault; leave room for a slow fetch.
	codexAuthTimeoutMS = 30000
	// codex caches the key and re-runs keyget on this interval, so a rotated or
	// round-robin key takes effect per interval rather than per request.
	codexAuthRefreshMS = 300000
)

// codexConfig is the CODEX_HOME/config.toml cc-fleet owns. Named structs keep
// the encoded key order, so a rewrite with unchanged inputs is byte-identical.
type codexConfig struct {
	ModelProvider  string              `toml:"model_provider"`
	ModelProviders codexModelProviders `toml:"model_providers"`
	History        codexHistory        `toml:"history"`
}

type codexModelProviders struct {
	CCFleet codexModelProvider `toml:"ccfleet"`
}

type codexModelProvider struct {
	Name    string    `toml:"name"`
	BaseURL string    `toml:"base_url"`
	WireAPI string    `toml:"wire_api"`
	Auth    codexAuth `toml:"auth"`
}

type codexAuth struct {
	Command           string   `toml:"command"`
	Args              []string `toml:"args"`
	TimeoutMS         int      `toml:"timeout_ms"`
	RefreshIntervalMS int      `toml:"refresh_interval_ms"`
}

type codexHistory struct {
	Persistence string `toml:"persistence"`
}

// CodexHomePath returns the absolute CODEX_HOME directory for provider:
// <ConfigDir>/codex-home/<provider>. It sits beside providers.toml rather than
// under ~/.claude because codex, not Claude Code, reads it. Absolute so a
// worker launched in another working directory still finds it.
func CodexHomePath(provider string) (string, error) {
	if provider == "" {
		return "", errors.New("profile: provider is empty")
	}
	if err := ids.ValidateProviderName(provider); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	cfgDir, err = filepath.Abs(cfgDir)
	if err != nil {
		return "", fmt.Errorf("profile: resolve config dir: %w", err)
	}
	root := filepath.Join(cfgDir, codexHomeDirName)
	path := filepath.Join(root, provider)
	if err := ids.EnsureUnderRoot(root, path); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return path, nil
}

// GenerateCodexConfig renders the config.toml that points codex at v's
// conversion daemon, with `<helperBinary> keyget <provider>` as the auth
// command: the key is fetched at run time and never written here. codex execs
// the command directly (no shell), so nothing is quoted.
func GenerateCodexConfig(v *config.Provider, helperBinary string) ([]byte, error) {
	if v == nil {
		return nil, errors.New("profile: nil provider")
	}
	if v.Name == "" {
		return nil, errors.New("profile: provider name is empty")
	}
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
	// The daemon derives its listen port the same way, and binds 127.0.0.1 only,
	// so a "localhost" base_url (which may resolve to ::1) is pinned to it.
	port, err := codexproxy.PortFromBaseURL(v.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("profile: provider %q: %w", v.Name, err)
	}

	cfg := codexConfig{
		ModelProvider: codexProviderID,
		ModelProviders: codexModelProviders{CCFleet: codexModelProvider{
			Name:    "cc-fleet " + v.Name,
			BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
			WireAPI: "responses",
			Auth: codexAuth{
				Command:           helperBinary,
				Args:              []string{"keyget", v.Name},
				TimeoutMS:         codexAuthTimeoutMS,
				RefreshIntervalMS: codexAuthRefreshMS,
			},
		}},
		History: codexHistory{Persistence: "none"},
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return nil, fmt.Errorf("profile: encode codex config for %q: %w", v.Name, err)
	}
	return buf.Bytes(), nil
}

// WriteCodexHome writes v's CODEX_HOME/config.toml (dir 0700, file 0600) and
// returns the directory. An empty helperBinary means os.Executable(). A file
// whose bytes already match is left untouched, so concurrent callers need no
// lock. codex keeps its own runtime state in the same directory.
func WriteCodexHome(v *config.Provider, helperBinary string) (string, error) {
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

	data, err := GenerateCodexConfig(v, helperBinary)
	if err != nil {
		return "", err
	}

	dir, err := CodexHomePath(v.Name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "config.toml")
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("profile: mkdir %s: %w", dir, err)
	}
	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return "", fmt.Errorf("profile: write %s: %w", path, err)
	}
	return dir, nil
}
