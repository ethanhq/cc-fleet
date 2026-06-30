package onboarding

import (
	"runtime"

	"github.com/ethanhq/cc-fleet/internal/ccver"
)

// NeedsClaudeInstall reports whether a bare-interactive launch should offer to
// install Claude Code: no `claude` binary is locatable AND the user hasn't
// dismissed the offer for good. Presence is probed without executing claude
// (ccver.Located), so the common case — claude already present — costs no
// subprocess.
//
// Unlike the agent-teams nudge this is NOT gated off on Windows: `claude` backs
// the subagent / workflow / run lanes on every platform.
func NeedsClaudeInstall() bool {
	if _, ok := ccver.Located(); ok {
		return false
	}
	st, _ := LoadState()
	return !st.ClaudeInstallAck
}

// ClaudeInstallCommand returns the command that installs Claude Code on the
// current OS, plus the canonical one-liner to show the user. The EXECUTED form is
// fail-closed (a bare `curl … | bash` reports success even when the download
// fails, because a POSIX pipeline takes its status from the last command); the
// DISPLAY form is the clean command cc-fleet's README documents.
func ClaudeInstallCommand() (name string, args []string, display string) {
	return claudeInstallCommandForGOOS(runtime.GOOS)
}

func claudeInstallCommandForGOOS(goos string) (name string, args []string, display string) {
	if goos == "windows" {
		return "powershell",
			[]string{"-NoProfile", "-Command", "$ErrorActionPreference = 'Stop'; irm https://claude.ai/install.ps1 | iex"},
			"irm https://claude.ai/install.ps1 | iex"
	}
	return "bash",
		[]string{"-c", "set -o pipefail; curl -fsSL https://claude.ai/install.sh | bash"},
		"curl -fsSL https://claude.ai/install.sh | bash"
}
