//go:build !windows

package codexproxy

import (
	"os/exec"
	"syscall"
)

// detach makes the spawned daemon its own process-group leader so it outlives the
// cc-fleet process that started it (mirrors subagent's Setpgid detach).
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
