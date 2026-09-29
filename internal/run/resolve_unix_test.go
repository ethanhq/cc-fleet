//go:build !windows

package run

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNoFingerprintJSONRead: the production resolveBinary (claudebin) ignores a
// leftover ~/.config/cc-fleet/fingerprint.json — whether its recorded binary is
// gone or still on disk, the claude on PATH is what runs.
func TestNoFingerprintJSONRead(t *testing.T) {
	pathDir := t.TempDir()
	onPath := filepath.Join(pathDir, "claude")
	if err := os.WriteFile(onPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for name, recorded := range map[string]string{
		"recorded binary missing": filepath.Join(t.TempDir(), "versions", "2.1.1", "claude"),
		"recorded binary present": decoy,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("PATH", pathDir)
			cfgDir := filepath.Join(home, ".config", "cc-fleet")
			if err := os.MkdirAll(cfgDir, 0o700); err != nil {
				t.Fatal(err)
			}
			fp := `{"cc_version":"2.1.1","binary_path":"` + recorded + `","env":{},"flags_template":[]}`
			if err := os.WriteFile(filepath.Join(cfgDir, "fingerprint.json"), []byte(fp), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := resolveBinary()
			if err != nil {
				t.Fatalf("resolveBinary: %v", err)
			}
			if got != onPath {
				t.Fatalf("resolveBinary = %q, want the PATH claude %q (fingerprint.json must not be read)", got, onPath)
			}
		})
	}
}
