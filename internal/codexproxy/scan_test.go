package codexproxy

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestScanDefaultModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := ScanDefaultModel("fallback"); got != "fallback" {
		t.Fatalf("absent config.toml: %q", got)
	}
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("model = \"gpt-5.5\"\nmodel_reasoning_effort = \"xhigh\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ScanDefaultModel("fallback"); got != "gpt-5.5" {
		t.Fatalf("scanned model: %q", got)
	}
}

func TestChoosePort(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no daemon state file
	t.Setenv("HOME", t.TempDir())

	// A held port (not ours) is rejected when explicitly preferred.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	held := ln.Addr().(*net.TCPAddr).Port
	if _, err := ChoosePort(held); err == nil {
		t.Fatalf("held port %d must be rejected", held)
	}

	// A free explicit preference is honored.
	free := freePort(t)
	if got, err := ChoosePort(free); err != nil || got != free {
		t.Fatalf("ChoosePort(%d) = %d, %v", free, got, err)
	}

	// Auto-pick lands in the reserved range.
	got, err := ChoosePort(0)
	if err != nil {
		t.Fatal(err)
	}
	if got < defaultPortBase || got >= defaultPortBase+portScanWidth {
		t.Fatalf("auto port %d outside the reserved range", got)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}
