//go:build !windows

package codexproxy

import (
	"os/exec"
	"syscall"
)

// detach starts the daemon in its own session (and so its own process group) so
// it outlives the cc-fleet process that started it and the terminal or tmux pane
// that process ran in: closing the pane hangs up that pane's session, not this one.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
