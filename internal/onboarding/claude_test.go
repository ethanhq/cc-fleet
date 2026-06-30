package onboarding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClaudeInstallCommandForGOOS(t *testing.T) {
	t.Run("unix", func(t *testing.T) {
		name, args, display := claudeInstallCommandForGOOS("linux")
		if name != "bash" {
			t.Fatalf("name = %q, want bash", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "set -o pipefail") {
			t.Fatalf("executed args must be fail-closed (pipefail): %q", joined)
		}
		if !strings.Contains(joined, "https://claude.ai/install.sh") {
			t.Fatalf("executed args missing installer URL: %q", joined)
		}
		if strings.Contains(display, "pipefail") || display != "curl -fsSL https://claude.ai/install.sh | bash" {
			t.Fatalf("display must be the clean canonical command, got %q", display)
		}
	})

	t.Run("windows", func(t *testing.T) {
		name, args, display := claudeInstallCommandForGOOS("windows")
		if name != "powershell" {
			t.Fatalf("name = %q, want powershell", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "ErrorActionPreference") {
			t.Fatalf("executed args must be fail-closed (Stop preference): %q", joined)
		}
		if !strings.Contains(joined, "https://claude.ai/install.ps1") {
			t.Fatalf("executed args missing installer URL: %q", joined)
		}
		if display != "irm https://claude.ai/install.ps1 | iex" {
			t.Fatalf("display wrong: %q", display)
		}
	})
}

func TestNeedsClaudeInstall(t *testing.T) {
	binName := "claude"
	if runtime.GOOS == "windows" {
		binName = "claude.exe"
	}

	t.Run("absent_no_ack_offers", func(t *testing.T) {
		setupHome(t)
		t.Setenv("PATH", t.TempDir())
		if !NeedsClaudeInstall() {
			t.Fatal("claude absent + no ack → want NeedsClaudeInstall true")
		}
	})

	t.Run("absent_with_ack_suppressed", func(t *testing.T) {
		setupHome(t)
		t.Setenv("PATH", t.TempDir())
		if err := (State{ClaudeInstallAck: true}).Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if NeedsClaudeInstall() {
			t.Fatal("ack set → want NeedsClaudeInstall false even while claude is absent")
		}
	})

	t.Run("present_suppressed", func(t *testing.T) {
		setupHome(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, binName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		if NeedsClaudeInstall() {
			t.Fatal("claude present → want NeedsClaudeInstall false")
		}
	})
}
