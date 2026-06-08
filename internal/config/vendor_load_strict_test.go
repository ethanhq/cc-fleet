package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadFromPath_RejectsTypoKeyRotation: a typo in a closed-enum field must be
// rejected at Load, not silently fall through to the dispatch's default branch
// (which would silently pick the first-enabled key while the user thinks
// rotation is on). The proof case is `key_rotation = "typo"`.
func TestLoadFromPath_RejectsTypoKeyRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 1

[deepseek]
base_url = "https://api.deepseek.com/anthropic"
default_model = "deepseek-flash"
models_endpoint = "https://api.deepseek.com/v1/models"
secret_backend = "file"
secret_ref = "deepseek.key"
enabled = true
added_at = 2026-05-24T05:00:00Z
key_rotation = "typo"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("LoadFromPath: want error for key_rotation=typo, got nil")
	}
	if !strings.Contains(err.Error(), "key_rotation") || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("err %q should name field + value", err.Error())
	}
}

// TestLoadFromPath_RejectsUnknownSecretBackend: Load default-strict surfaces a
// secret_backend typo at the first config read, instead of only at keyget time
// (by when the user may be partway through a spawn).
func TestLoadFromPath_RejectsUnknownSecretBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 1

[weirdo]
base_url = "https://api.example.com/anthropic"
default_model = "x"
models_endpoint = "https://api.example.com/v1/models"
secret_backend = "wired"
secret_ref = "x.key"
enabled = true
added_at = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("LoadFromPath: want error for secret_backend=wired, got nil")
	}
	if !strings.Contains(err.Error(), "secret_backend") || !strings.Contains(err.Error(), "wired") {
		t.Fatalf("err %q should call out field + bad value", err.Error())
	}
}

// A codex-oauth vendor with a remote base_url must be rejected at load — the
// loopback handshake secret must never be sent off-host.
func TestLoadFromPath_RejectsRemoteCodexBaseURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 1

[codex]
base_url = "https://evil.example.com/"
default_model = "gpt-5.5"
models_endpoint = "http://127.0.0.1:17222/v1/models"
secret_backend = "codex-oauth"
secret_ref = "codex-oauth"
enabled = true
added_at = 2026-06-08T05:00:00Z
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("LoadFromPath: want error for a remote codex base_url, got nil")
	}
	if !strings.Contains(err.Error(), "codex base_url") || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("err %q should call out the codex loopback requirement", err.Error())
	}
}

// A codex-oauth vendor with loopback endpoints loads cleanly.
func TestLoadFromPath_AcceptsLoopbackCodex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 1

[codex]
base_url = "http://127.0.0.1:17222/"
default_model = "gpt-5.5"
models_endpoint = "http://127.0.0.1:17222/v1/models"
secret_backend = "codex-oauth"
secret_ref = "codex-oauth"
enabled = true
added_at = 2026-06-08T05:00:00Z
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadFromPath(path); err != nil {
		t.Fatalf("loopback codex vendor must load, got %v", err)
	}
}

// TestLoadFromPath_RejectsEmptySecretRef: every vendor needs a secret_ref so
// keyget knows what to fetch.
func TestLoadFromPath_RejectsEmptySecretRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 1

[deepseek]
base_url = "https://api.deepseek.com/anthropic"
default_model = "deepseek-flash"
models_endpoint = "https://api.deepseek.com/v1/models"
secret_backend = "file"
secret_ref = ""
enabled = true
added_at = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("LoadFromPath: want error for empty secret_ref")
	}
	if !strings.Contains(err.Error(), "secret_ref") {
		t.Fatalf("err %q should mention secret_ref", err.Error())
	}
}

// TestLoadFromPath_RejectsWrongVersion: schema-version drift is one of the
// hardest classes of bug to debug at runtime; surface it at Load.
func TestLoadFromPath_RejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vendors.toml")
	body := `version = 99

[deepseek]
base_url = "https://api.deepseek.com/anthropic"
default_model = "deepseek-flash"
models_endpoint = "https://api.deepseek.com/v1/models"
secret_backend = "file"
secret_ref = "deepseek.key"
enabled = true
added_at = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFromPath(path)
	if err == nil {
		t.Fatal("LoadFromPath: want error for version=99")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("err %q should mention version", err.Error())
	}
}

// TestLoadFromPath_RejectsMaliciousTableName: a hand-edited vendors.toml table
// name that's path-traversal ("../escape") or shell-injection ("bad;touch x")
// shaped must be rejected at Load — the name becomes Vendor.Name and flows into
// profile.ProfilePath (filepath.Join → traversal) and the apiKeyHelper command
// (shell-evaluated → injection).
func TestLoadFromPath_RejectsMaliciousTableName(t *testing.T) {
	cases := []struct {
		name, table string
	}{
		{"path-traversal", "../escape"},
		{"shell-injection", "bad;touch x"},
		{"leading-hyphen", "-rm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "vendors.toml")
			body := `version = 1

["` + tc.table + `"]
base_url = "https://api.example.com/anthropic"
default_model = "x"
models_endpoint = "https://api.example.com/v1/models"
secret_backend = "file"
secret_ref = "x.key"
enabled = true
added_at = 2026-05-24T05:00:00Z
`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := LoadFromPath(path)
			if err == nil {
				t.Fatalf("LoadFromPath: want error for malicious table name %q, got nil", tc.table)
			}
			if !strings.Contains(err.Error(), "vendor name") {
				t.Fatalf("err %q should call out the invalid vendor name", err.Error())
			}
		})
	}
}

// TestLoadFromPath_MissingFileStillReturnsEmpty: backward-compatible — a
// non-existent vendors.toml still returns a fresh empty Config (every
// existing test relies on this for first-run scaffolding).
func TestLoadFromPath_MissingFileStillReturnsEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.toml")
	cfg, err := LoadFromPath(missing)
	if err != nil {
		t.Fatalf("LoadFromPath(missing): %v", err)
	}
	if cfg == nil || cfg.Version != SchemaVersion || len(cfg.Vendors) != 0 {
		t.Fatalf("LoadFromPath(missing) = %+v, want empty Config", cfg)
	}
}
