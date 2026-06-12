package codexproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// The native leaf must not touch provider config: a malformed providers.toml
// (which fails config.Load for every provider) never gates or side-effects it.
func TestEnsureForProviderName_ReservedNativeNoop(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureForProviderName(config.ReservedNativeProvider); err != nil {
		t.Fatalf("reserved native preflight must be a config-free no-op, got %v", err)
	}
	if err := EnsureForProviderName("glm"); err == nil {
		t.Fatal("a real provider name must still surface the config-load failure")
	}
}
