package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// resolveLeadSession enforces the launcher precedence: an explicit flag wins, else
// the parent Claude session, else the Codex launcher id (CODEX_THREAD_ID), else "".
func TestResolveLeadSession(t *testing.T) {
	orig := detectLeadSession
	t.Cleanup(func() { detectLeadSession = orig })

	// An explicit flag wins over both a live Claude session and a codex thread.
	detectLeadSession = func() string { return "claude-sess" }
	t.Setenv("CODEX_THREAD_ID", "thread-xyz")
	if got := resolveLeadSession("explicit-1"); got != "explicit-1" {
		t.Fatalf("explicit flag should win, got %q", got)
	}

	// No flag → the Claude session beats the codex thread.
	if got := resolveLeadSession(""); got != "claude-sess" {
		t.Fatalf("Claude session should beat codex thread, got %q", got)
	}

	// No flag and no Claude session → fall back to the codex launcher id.
	detectLeadSession = func() string { return "" }
	if got := resolveLeadSession(""); got != "codex:thread-xyz" {
		t.Fatalf("should fall back to codex thread, got %q", got)
	}

	// Nothing available → "" (the board's "(no session)").
	t.Setenv("CODEX_THREAD_ID", "")
	if got := resolveLeadSession(""); got != "" {
		t.Fatalf("no launcher should yield empty, got %q", got)
	}
}

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
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	ensureProviderProxy = func(*config.Provider, *diag.Logger) error {
		return errors.New("codex proxy did not become ready on port 17222")
	}
	t.Cleanup(func() { ensureProviderProxy = codexproxy.EnsureForProvider })

	res := Run(context.Background(), Request{Provider: "codex", Prompt: "hi", JSON: true})
	if res.OK || res.ErrorCode != ErrCodeProxyUnavailable {
		t.Fatalf("want CODEX_PROXY_UNAVAILABLE, got ok=%v code=%s msg=%s", res.OK, res.ErrorCode, res.ErrorMsg)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "profiles", "codex.json")); !os.IsNotExist(err) {
		t.Fatalf("profile must not be written on daemon failure (stat err=%v)", err)
	}
}
