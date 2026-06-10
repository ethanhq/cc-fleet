//go:build windows

package tui

import "os"

// pidAliveForSweep reports whether pid is live. os.FindProcess on Windows
// opens the process and errors when it does not exist; erring toward "alive"
// only delays a sweep, never removes a live session's file.
func pidAliveForSweep(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
