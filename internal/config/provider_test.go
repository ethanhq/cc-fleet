package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// validProvider returns a Provider that should pass Validate. Tests mutate copies
// of this to exercise individual failure modes.
func validProvider(name string) *Provider {
	return &Provider{
		Name:           name,
		BaseURL:        "https://api." + name + ".com/anthropic",
		DefaultModel:   name + "-flash",
		ModelsEndpoint: "https://api." + name + ".com/v1/models",
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        true,
		AddedAt:        time.Date(2026, 5, 24, 5, 0, 0, 0, time.UTC),
	}
}

func TestRoundTrip(t *testing.T) {
	cfg := &Config{
		Version: SchemaVersion,
		Providers: map[string]*Provider{
			"deepseek": validProvider("deepseek"),
			"kimi": {
				Name:           "kimi",
				BaseURL:        "https://api.moonshot.cn/anthropic",
				DefaultModel:   "kimi-latest",
				ModelsEndpoint: "https://api.moonshot.cn/anthropic/v1/models",
				SecretBackend:  "pass",
				SecretRef:      "moonshot/kimi-key",
				Enabled:        true,
				AddedAt:        time.Date(2026, 5, 24, 6, 30, 0, 0, time.UTC),
			},
			"glm": {
				Name:           "glm",
				BaseURL:        "https://open.bigmodel.cn/api/anthropic",
				DefaultModel:   "glm-4.6",
				ModelsEndpoint: "https://open.bigmodel.cn/api/paas/v4/models",
				SecretBackend:  "1password",
				SecretRef:      "op://Personal/glm/credential",
				Enabled:        false,
				AddedAt:        time.Date(2026, 5, 24, 7, 0, 0, 0, time.UTC),
			},
		},
	}

	path := filepath.Join(t.TempDir(), "providers.toml")
	if err := SaveToPath(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Version != cfg.Version {
		t.Fatalf("version mismatch: got %d want %d", got.Version, cfg.Version)
	}
	if len(got.Providers) != len(cfg.Providers) {
		t.Fatalf("provider count: got %d want %d", len(got.Providers), len(cfg.Providers))
	}
	for name, want := range cfg.Providers {
		gv, ok := got.Providers[name]
		if !ok {
			t.Fatalf("missing provider %q after round-trip", name)
		}
		if gv.Name != want.Name {
			t.Errorf("%s: Name = %q, want %q", name, gv.Name, want.Name)
		}
		if gv.BaseURL != want.BaseURL {
			t.Errorf("%s: BaseURL = %q, want %q", name, gv.BaseURL, want.BaseURL)
		}
		if gv.DefaultModel != want.DefaultModel {
			t.Errorf("%s: DefaultModel = %q, want %q", name, gv.DefaultModel, want.DefaultModel)
		}
		if gv.ModelsEndpoint != want.ModelsEndpoint {
			t.Errorf("%s: ModelsEndpoint = %q, want %q", name, gv.ModelsEndpoint, want.ModelsEndpoint)
		}
		if gv.SecretBackend != want.SecretBackend {
			t.Errorf("%s: SecretBackend = %q, want %q", name, gv.SecretBackend, want.SecretBackend)
		}
		if gv.SecretRef != want.SecretRef {
			t.Errorf("%s: SecretRef = %q, want %q", name, gv.SecretRef, want.SecretRef)
		}
		if gv.Enabled != want.Enabled {
			t.Errorf("%s: Enabled = %v, want %v", name, gv.Enabled, want.Enabled)
		}
		if !gv.AddedAt.Equal(want.AddedAt) {
			t.Errorf("%s: AddedAt = %v, want %v", name, gv.AddedAt, want.AddedAt)
		}
	}
}

func TestValidate_Missing(t *testing.T) {
	// Each sub-case zeroes one required field on an otherwise valid provider.
	cases := []struct {
		name    string
		mutate  func(*Provider)
		wantSub string // substring expected in the error
	}{
		{"BaseURL", func(v *Provider) { v.BaseURL = "" }, "base_url is required"},
		{"DefaultModel", func(v *Provider) { v.DefaultModel = "" }, "default_model is required"},
		{"ModelsEndpoint", func(v *Provider) { v.ModelsEndpoint = "" }, "models_endpoint is required"},
		{"SecretRef", func(v *Provider) { v.SecretRef = "" }, "secret_ref is required"},
		{"SecretBackend", func(v *Provider) { v.SecretBackend = "" }, "secret_backend is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validProvider("deepseek")
			tc.mutate(v)
			cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": v}}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidate_BadEnum(t *testing.T) {
	v := validProvider("deepseek")
	v.SecretBackend = "wat"
	cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": v}}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("Validate: want error, got nil")
	}
	if !strings.Contains(err.Error(), "secret_backend") || !strings.Contains(err.Error(), "wat") {
		t.Fatalf("error %q should mention bad enum value", err.Error())
	}
}

func TestValidate_BadURL(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Provider)
		wantSub string
	}{
		{"unparseable_base_url", func(v *Provider) { v.BaseURL = "://nope" }, "base_url"},
		{"non_http_scheme", func(v *Provider) { v.BaseURL = "ftp://x.com" }, "base_url"},
		{"missing_host", func(v *Provider) { v.BaseURL = "https://" }, "base_url"},
		{"bad_models_endpoint", func(v *Provider) { v.ModelsEndpoint = "ftp://x.com" }, "models_endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validProvider("deepseek")
			tc.mutate(v)
			cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": v}}
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidate_BadVersion(t *testing.T) {
	// Bonus: version != 1 must error so future migrations have a hook.
	cfg := &Config{Version: 2, Providers: map[string]*Provider{}}
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate: want error for version=2, got nil")
	}
}

func TestValidate_KeyRotation(t *testing.T) {
	for _, ok := range []string{"", "off", "round_robin", "random"} {
		v := validProvider("deepseek")
		v.KeyRotation = ok
		cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": v}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(key_rotation=%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"rotate", "Random", "robin", "ROUND_ROBIN"} {
		v := validProvider("deepseek")
		v.KeyRotation = bad
		cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": v}}
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate(key_rotation=%q): want error, got nil", bad)
		}
		if !strings.Contains(err.Error(), "key_rotation") || !strings.Contains(err.Error(), bad) {
			t.Fatalf("error %q should call out the bad key_rotation value", err.Error())
		}
	}
}

func TestKeyRotation_OmitemptyAndOldFileParse(t *testing.T) {
	// off (empty) must NOT write a key_rotation line — single-key users' files
	// stay byte-identical to the pre-multi-key layout.
	offPath := filepath.Join(t.TempDir(), "off.toml")
	offCfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": validProvider("deepseek")}}
	if err := SaveToPath(offCfg, offPath); err != nil {
		t.Fatalf("SaveToPath(off): %v", err)
	}
	body, err := os.ReadFile(offPath)
	if err != nil {
		t.Fatalf("read off.toml: %v", err)
	}
	if strings.Contains(string(body), "key_rotation") {
		t.Fatalf("off provider wrote a key_rotation line:\n%s", body)
	}

	// A non-off strategy round-trips through the file.
	rrPath := filepath.Join(t.TempDir(), "rr.toml")
	rrProvider := validProvider("deepseek")
	rrProvider.KeyRotation = "round_robin"
	rrCfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"deepseek": rrProvider}}
	if err := SaveToPath(rrCfg, rrPath); err != nil {
		t.Fatalf("SaveToPath(rr): %v", err)
	}
	rrBody, _ := os.ReadFile(rrPath)
	if !strings.Contains(string(rrBody), "key_rotation = \"round_robin\"") {
		t.Fatalf("round_robin provider missing key_rotation line:\n%s", rrBody)
	}
	got, err := LoadFromPath(rrPath)
	if err != nil {
		t.Fatalf("reload rr: %v", err)
	}
	if got.Providers["deepseek"].KeyRotation != "round_robin" {
		t.Fatalf("reloaded key_rotation = %q, want round_robin", got.Providers["deepseek"].KeyRotation)
	}

	// An OLD providers.toml that predates key_rotation parses fine and defaults
	// to off (empty) — backward compatibility, SchemaVersion unchanged.
	oldBody := `version = 1

[deepseek]
base_url = "https://api.deepseek.com/anthropic"
default_model = "deepseek-flash"
models_endpoint = "https://api.deepseek.com/v1/models"
secret_backend = "file"
secret_ref = "deepseek.key"
enabled = true
added_at = 2026-05-24T05:00:00Z
`
	oldPath := filepath.Join(t.TempDir(), "old.toml")
	if err := os.WriteFile(oldPath, []byte(oldBody), 0o600); err != nil {
		t.Fatalf("write old.toml: %v", err)
	}
	oldCfg, err := LoadFromPath(oldPath)
	if err != nil {
		t.Fatalf("LoadFromPath(old): %v", err)
	}
	if v := oldCfg.Providers["deepseek"]; v == nil || v.KeyRotation != "" {
		t.Fatalf("old file key_rotation = %q, want \"\" (off)", v.KeyRotation)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.toml")
	cfg, err := LoadFromPath(path)
	if err != nil {
		t.Fatalf("LoadFromPath: want nil error for missing file, got %v", err)
	}
	if cfg == nil {
		t.Fatalf("LoadFromPath: got nil Config")
	}
	if cfg.Version != SchemaVersion {
		t.Fatalf("Version = %d, want %d", cfg.Version, SchemaVersion)
	}
	if cfg.Providers == nil || len(cfg.Providers) != 0 {
		t.Fatalf("Providers = %v, want empty map", cfg.Providers)
	}
}

func TestSave_Perm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not meaningful on Windows")
	}
	cfg := &Config{
		Version:   SchemaVersion,
		Providers: map[string]*Provider{"deepseek": validProvider("deepseek")},
	}
	path := filepath.Join(t.TempDir(), "providers.toml")
	if err := SaveToPath(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// We only care about the low 9 permission bits.
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
}

func TestSave_RejectsInvalid(t *testing.T) {
	// Save must validate before writing — a half-built Config should never
	// land on disk.
	bad := &Config{
		Version:   SchemaVersion,
		Providers: map[string]*Provider{"x": {Name: "x"}}, // all required fields empty
	}
	path := filepath.Join(t.TempDir(), "providers.toml")
	if err := SaveToPath(bad, path); err == nil {
		t.Fatalf("SaveToPath: want validation error, got nil")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file unexpectedly created: stat err = %v", err)
	}
}

func TestSave_Atomic_NoTempLeft(t *testing.T) {
	// After a successful Save, only providers.toml should remain — no .tmp files.
	cfg := &Config{
		Version:   SchemaVersion,
		Providers: map[string]*Provider{"deepseek": validProvider("deepseek")},
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.toml")
	if err := SaveToPath(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

func TestLocations_XDG(t *testing.T) {
	// Verify ConfigDir honors XDG_CONFIG_HOME first, falls back to HOME, and
	// errors when neither is set.
	t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
	t.Setenv("HOME", "/home/user")
	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if want := filepath.Join("/custom/xdg", "cc-fleet"); got != want {
		t.Fatalf("ConfigDir = %q, want %q", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	got, err = ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if want := filepath.Join("/home/user", ".config", "cc-fleet"); got != want {
		t.Fatalf("ConfigDir = %q, want %q", got, want)
	}

	t.Setenv("HOME", "")
	if _, err := ConfigDir(); err == nil {
		t.Fatalf("ConfigDir: want error when neither XDG_CONFIG_HOME nor HOME set")
	}
}

func TestLocations_ProvidersAndSecretsPaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
	t.Setenv("HOME", "/home/user")
	vp, err := ProvidersPath()
	if err != nil {
		t.Fatalf("ProvidersPath: %v", err)
	}
	if want := filepath.Join("/tmp/xdg-test", "cc-fleet", "providers.toml"); vp != want {
		t.Fatalf("ProvidersPath = %q, want %q", vp, want)
	}
	sd, err := SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if want := filepath.Join("/tmp/xdg-test", "cc-fleet", "secrets"); sd != want {
		t.Fatalf("SecretsDir = %q, want %q", sd, want)
	}
}
