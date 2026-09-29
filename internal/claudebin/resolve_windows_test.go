//go:build windows

package claudebin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveWindowsClaudeExe: under %USERPROFILE%'s per-version layout a
// non-empty claude.exe with 0644 permissions is accepted (windows has no x bit);
// once it is truncated to 0 bytes Resolve reports ErrNotFound.
func TestResolveWindowsClaudeExe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("PATH", t.TempDir()) // no claude on PATH: the layout fallback runs

	exe := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.150", "claude.exe")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, ver, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if path != exe {
		t.Fatalf("path = %q, want %q", path, exe)
	}
	if ver != "2.1.150" {
		t.Fatalf("version = %q, want 2.1.150", ver)
	}

	if err := os.WriteFile(exe, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Resolve(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve with a 0-byte claude.exe: err = %v, want ErrNotFound", err)
	}
}
