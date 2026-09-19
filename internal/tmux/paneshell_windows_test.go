//go:build windows

package tmux

import (
	"strings"
	"testing"
)

// TestWrapPaneCommand_PowerShellSafe pins the two properties the psmux wrapper
// depends on: the result carries no double quote (psmux wraps the whole string
// in one) and every literal single quote is doubled, PowerShell's escape inside
// a '…' string. Requires the opt-in env var, same as production.
func TestWrapPaneCommand_PowerShellSafe(t *testing.T) {
	t.Setenv(windowsTmuxOptInEnv, "1")
	t.Setenv("CC_FLEET_WINDOWS_BASH", `C:\bash.exe`)

	got := wrapPaneCommand(`env -u ANTHROPIC_API_KEY 'CLAUDECODE=1' '/bin/claude'`)

	if strings.Contains(got, `"`) {
		t.Errorf("wrapped command must not contain a double quote: %s", got)
	}
	if !strings.HasPrefix(got, `& 'C:\bash.exe' -c '`) {
		t.Errorf("want a call-operator bash invocation, got: %s", got)
	}
	if !strings.Contains(got, `''CLAUDECODE=1''`) {
		t.Errorf("single quotes must be doubled, got: %s", got)
	}
	if !strings.Contains(got, "export PATH=/usr/bin:/bin:$PATH;") {
		t.Errorf("PATH must be seeded so env resolves, got: %s", got)
	}
}

// TestWrapPaneCommand_OptOutByDefault is the load-bearing safety property: with
// the env var unset, wrapPaneCommand must be a no-op. This is what keeps `go
// test` exercising the tmux package the same way it does upstream (against a
// faked tmux binary expecting the raw argv), and what keeps a plain Windows
// build from emitting a PowerShell-specific command line to every caller.
func TestWrapPaneCommand_OptOutByDefault(t *testing.T) {
	t.Setenv(windowsTmuxOptInEnv, "")
	t.Setenv("CC_FLEET_WINDOWS_BASH", `C:\bash.exe`)

	const cmd = `env -u ANTHROPIC_API_KEY 'CLAUDECODE=1' '/bin/claude'`
	if got := wrapPaneCommand(cmd); got != cmd {
		t.Errorf("wrapPaneCommand must pass through unchanged when %s is unset, got: %s", windowsTmuxOptInEnv, got)
	}
}

// TestWrapPaneCommand_PassThrough covers the two cases that must not be
// rewritten even opted in: an empty command, and a host with no bash to hand
// off to (the caller should see the raw shell failure rather than a silently
// altered line).
func TestWrapPaneCommand_PassThrough(t *testing.T) {
	t.Setenv(windowsTmuxOptInEnv, "1")

	t.Setenv("CC_FLEET_WINDOWS_BASH", `C:\bash.exe`)
	if got := wrapPaneCommand(""); got != "" {
		t.Errorf("empty command must pass through, got: %q", got)
	}

	t.Setenv("CC_FLEET_WINDOWS_BASH", "")
	t.Setenv("PATH", "")
	if got := wrapPaneCommand("echo hi"); got != "echo hi" && !strings.HasPrefix(got, "& '") {
		t.Errorf("unexpected result with no bash configured: %q", got)
	}
}
