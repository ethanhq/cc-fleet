package ccver

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLocated(t *testing.T) {
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName = "claude.exe"
	}

	t.Run("absent", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		t.Setenv("PATH", t.TempDir()) // an empty dir — nothing resolvable on PATH
		if p, ok := Located(); ok {
			t.Fatalf("Located() = %q, true with no claude installed; want \"\", false", p)
		}
	})

	t.Run("present_on_path", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, binName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		p, ok := Located()
		if !ok {
			t.Fatal("Located() = _, false; want the seeded claude found")
		}
		if p == "" {
			t.Fatal("Located() reported ok with an empty path")
		}
	})
}
