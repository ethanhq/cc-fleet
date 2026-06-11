package profile

import (
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// TestProfilePath_RejectsMaliciousName is the defense-in-depth check: even if a
// malformed provider name reached ProfilePath (bypassing the config Load gate),
// the constructed path must never escape ~/.claude/profiles/. ProfilePath
// validates the grammar AND under-root-checks the joined path.
func TestProfilePath_RejectsMaliciousName(t *testing.T) {
	isolateHome(t)
	for _, name := range []string{"../escape", "../../etc/cron.d/x", "bad;touch x", "a/b"} {
		if _, err := ProfilePath(name); err == nil {
			t.Errorf("ProfilePath(%q): want error, got nil (path traversal!)", name)
		}
	}
}

// TestProfilePath_HappyPathStaysUnderRoot: a normal provider name resolves to a
// path inside ProfilesDir.
func TestProfilePath_HappyPathStaysUnderRoot(t *testing.T) {
	isolateHome(t)
	dir, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir: %v", err)
	}
	got, err := ProfilePath("deepseek")
	if err != nil {
		t.Fatalf("ProfilePath: %v", err)
	}
	if got != filepath.Join(dir, "deepseek.json") {
		t.Fatalf("ProfilePath = %q, want %q", got, filepath.Join(dir, "deepseek.json"))
	}
}

// TestGenerateForProvider_RejectsMaliciousName: the apiKeyHelper command
// concatenates the provider name; a shell-injection name must be refused so it
// can never reach the shell Claude Code hands the helper to.
func TestGenerateForProvider_RejectsMaliciousName(t *testing.T) {
	v := &config.Provider{
		Name:    "x; rm -rf /",
		BaseURL: "https://api.example.com/anthropic",
	}
	if _, err := GenerateForProvider(v, testHelperBin()); err == nil {
		t.Fatal("GenerateForProvider: want error for shell-injection provider name, got nil")
	}
}
