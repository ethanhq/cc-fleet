//go:build windows

package procintrospect

import (
	"errors"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUnsupported is returned by the argv/table readers: recovering another
// process's argv on Windows requires an undocumented PEB read, so Cmdline and
// ProcessTable stay unsupported and their callers degrade exactly as on any
// non-introspectable platform. Identity checks use ProcStart instead.
var ErrUnsupported = errors.New("procintrospect: argv/table introspection unsupported on windows")

func Cmdline(pid int) ([]string, error) { return nil, ErrUnsupported }

func ProcessTable() ([]Process, error) { return nil, ErrUnsupported }

func Children(pid int) []int { return nil }

// Ppid walks a Toolhelp32 process snapshot for pid's parent.
func Ppid(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			return int(e.ParentProcessID), true
		}
	}
	return 0, false
}

// ProcStart returns pid's kernel creation time (FILETIME rendered as decimal
// nanoseconds) — like the linux jiffies and darwin epoch-seconds tokens it is
// meaningful only for same-platform equality, and a recycled pid gets a new
// creation time, so equality is a sound PID-reuse guard.
func ProcStart(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return "", false
	}
	return strconv.FormatInt(creation.Nanoseconds(), 10), true
}
