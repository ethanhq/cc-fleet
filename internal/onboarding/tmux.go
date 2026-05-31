package onboarding

import "os/exec"

// tmuxAvailable reports whether a runnable tmux binary is on PATH. Unlike
// agent-teams (a Claude runtime state), tmux presence is something cc-fleet CAN
// observe reliably, so the tmux setup nudge keys off it directly.
func tmuxAvailable() bool {
	return exec.Command("tmux", "-V").Run() == nil
}

// NeedsTmuxSetup reports whether the first-run TUI should show the tmux setup
// screen: tmux isn't installed AND the user hasn't already chosen "skip —
// subagent mode only" (TmuxAck).
func NeedsTmuxSetup() bool {
	if tmuxAvailable() {
		return false
	}
	st, _ := LoadState()
	return !st.TmuxAck
}
