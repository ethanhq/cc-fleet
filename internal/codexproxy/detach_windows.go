//go:build windows

package codexproxy

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach starts the daemon in a new process group so it outlives the launcher and
// no console Ctrl event leaks from the parent (mirrors subagent's windows detach).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}
