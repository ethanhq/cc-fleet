//go:build !windows

package tui

import (
	"errors"
	"syscall"
)

// pidAliveForSweep reports whether pid is a live process: signal 0 probes
// existence without delivering anything; EPERM means alive-but-foreign,
// which still must not be swept.
func pidAliveForSweep(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
