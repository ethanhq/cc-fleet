package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// The reserved native name resolves without touching providers.toml — a
// malformed config must not brick the providerless escape hatch.
func TestResolveProviderArg_ReservedSkipsConfig(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte("not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}

	name, err := resolveProviderArg(config.ReservedNativeProvider)
	if err != nil || name != config.ReservedNativeProvider {
		t.Fatalf("resolveProviderArg(claude) = %q, %v — want passthrough with no config load", name, err)
	}
	// A provider name still hits the (broken) config and errors.
	if _, err := resolveProviderArg("glm"); err == nil {
		t.Fatal("a real provider name must still surface the config-load failure")
	}
}
