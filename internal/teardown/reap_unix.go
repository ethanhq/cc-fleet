//go:build !windows

package teardown

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Reap seams: tests substitute them so `go test` never signals a real process.
var (
	signalProc = func(pid int, sig syscall.Signal) error {
		return syscall.Kill(pid, sig)
	}
	// procReapGrace is how long reapProcess waits after SIGTERM before
	// escalating to SIGKILL.
	procReapGrace = 750 * time.Millisecond
)

// reapProcess terminates pid: SIGTERM, a grace period, then SIGKILL. The start
// token is re-checked before each signal, so a process that already exited, or
// a pid the kernel handed to someone else in the meantime, is never signalled.
// A process that is gone is success.
func reapProcess(pid int, start string) error {
	if !startMatches(pid, start) {
		return nil
	}
	if err := signalProc(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}
	time.Sleep(procReapGrace)
	if !startMatches(pid, start) {
		return nil
	}
	if err := signalProc(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL pid %d: %w", pid, err)
	}
	return nil
}

func startMatches(pid int, start string) bool {
	got, ok := procStartFn(pid)
	return ok && start != "" && got == start
}
