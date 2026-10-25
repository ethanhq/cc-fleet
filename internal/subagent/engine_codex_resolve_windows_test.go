//go:build windows

package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexResolveWindowsCmd: npm installs codex.cmd on windows; LookPath finds
// it through PATHEXT and --version runs through cmd.exe. A 0-byte one fails.
func TestCodexResolveWindowsCmd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	cmdPath := filepath.Join(dir, "codex.cmd")
	if err := os.WriteFile(cmdPath, []byte("@echo codex-cli 0.163.0\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, ver, err := codexEngine{}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !strings.EqualFold(path, cmdPath) {
		t.Fatalf("path = %q, want %q", path, cmdPath)
	}
	if ver != "0.163.0" {
		t.Fatalf("version = %q, want 0.163.0", ver)
	}

	empty := t.TempDir()
	t.Setenv("PATH", empty)
	if err := os.WriteFile(filepath.Join(empty, "codex.cmd"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (codexEngine{}).Resolve(); !errors.Is(err, errCodexNotFound) {
		t.Fatalf("Resolve with a 0-byte codex.cmd: err = %v, want errCodexNotFound", err)
	}
}
