//go:build windows

package codexproxy

import "golang.org/x/sys/windows"

// stopProcess is the fallback when an authenticated /shutdown is unavailable.
// Windows has no SIGINT for an unrelated process, so terminate it directly
// (mirrors internal/subagent's windows reaper).
func stopProcess(pid int) {
	if pid <= 0 {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.TerminateProcess(h, 1)
}
