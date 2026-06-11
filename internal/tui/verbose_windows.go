//go:build windows

package tui

import (
	"errors"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a running process
// (STILL_ACTIVE, 259).
const stillActive = 259

// pidAliveForSweep reports whether pid is live, deliberately erring toward ALIVE.
// It opens the process (PROCESS_QUERY_LIMITED_INFORMATION) and treats STILL_ACTIVE
// from GetExitCodeProcess as alive. When OpenProcess fails it returns true UNLESS
// the error is ERROR_INVALID_PARAMETER (no such pid → dead): access-denied on a
// live foreign or protected process must NOT read as dead. This asymmetry is the
// opposite of subagent's pidAlive (open-failure → dead) and is intentional — a
// sweeper must never delete a live session's log, whereas a false-alive only
// delays a sweep by one cycle.
func pidAliveForSweep(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return !errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true // queryable but unreadable → keep the file
	}
	return code == stillActive
}
