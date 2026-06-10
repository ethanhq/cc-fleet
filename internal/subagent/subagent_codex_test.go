package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// A codex daemon-ensure failure is fail-before-mutation: classified result and
// no profile file left behind.
func TestRun_CodexDaemonFailure_FailsBeforeProfileWrite(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Inject a fingerprint pointing at a placeholder binary so the gate passes and
	// 3b is reached. The gate only os.Stats the path, so any file works; a PATH
	// lookup would need a claude.exe on Windows.
	binPath := filepath.Join(t.TempDir(), "claude-2.1.150")
	if err := os.WriteFile(binPath, []byte("placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: binPath}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[codex]
base_url        = "http://127.0.0.1:17222/"
default_model   = "gpt-5.5"
models_endpoint = "http://127.0.0.1:17222/v1/models"
secret_backend  = "codex-oauth"
secret_ref      = "codex-oauth"
enabled         = true
added_at        = 2026-06-08T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "vendors.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	ensureVendorProxy = func(*config.Vendor) error {
		return errors.New("codex proxy did not become ready on port 17222")
	}
	t.Cleanup(func() { ensureVendorProxy = codexproxy.EnsureForVendor })

	res := Run(context.Background(), Request{Vendor: "codex", Prompt: "hi", JSON: true})
	if res.OK || res.ErrorCode != ErrCodeProxyUnavailable {
		t.Fatalf("want CODEX_PROXY_UNAVAILABLE, got ok=%v code=%s msg=%s", res.OK, res.ErrorCode, res.ErrorMsg)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "profiles", "codex.json")); !os.IsNotExist(err) {
		t.Fatalf("profile must not be written on daemon failure (stat err=%v)", err)
	}
}
