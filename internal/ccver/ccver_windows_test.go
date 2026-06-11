//go:build windows

package ccver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetect_VersionsLayoutWindows places claude.exe under %USERPROFILE%'s
// per-version layout and confirms locate finds it: the basename is claude.exe
// and a regular file counts as executable (extension-driven on windows).
func TestDetect_VersionsLayoutWindows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	// PATH must NOT contain a claude binary so the fallback runs.
	t.Setenv("PATH", t.TempDir())

	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	for _, v := range []string{"1.2.3", "2.1.150", "2.1.99"} {
		d := filepath.Join(versionsDir, v)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
		if err := os.WriteFile(filepath.Join(d, "claude.exe"), []byte("MZ"), 0o644); err != nil {
			t.Fatalf("write claude.exe: %v", err)
		}
	}

	path, ver, err := Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := filepath.Join(versionsDir, "2.1.150", "claude.exe")
	if path != want {
		t.Fatalf("path = %q, want %q (largest semver)", path, want)
	}
	if ver != "2.1.150" {
		t.Fatalf("version = %q, want 2.1.150", ver)
	}
}
