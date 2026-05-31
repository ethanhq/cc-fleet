//go:build linux

package procintrospect

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procRoot is the procfs mount point. A var so tests can point the readers at a
// fixture tree instead of the live /proc.
var procRoot = "/proc"

// Cmdline reads /proc/<pid>/cmdline (the kernel's NUL-separated argv) and
// returns it as an exact argv slice. The trailing NUL is trimmed so the split
// doesn't yield an empty tail token; an empty cmdline (kernel returns "") gives
// a nil slice. It returns a slice — not a space-joined string — so arguments
// that contain spaces (e.g. `--settings /home/u with space/.claude/...json`)
// survive.
func Cmdline(pid int) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil, nil
	}
	parts := bytes.Split(data, []byte{0})
	argv := make([]string, 0, len(parts))
	for _, p := range parts {
		argv = append(argv, string(p))
	}
	return argv, nil
}

// Children returns pid's immediate children by reading every
// /proc/<pid>/task/<tid>/children file (threads have separate children lists on
// Linux). Errors are swallowed — an unreadable branch just can't be descended.
func Children(pid int) []int {
	taskDir := filepath.Join(procRoot, strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	var kids []int
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(taskDir, e.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if n, err := strconv.Atoi(f); err == nil {
				kids = append(kids, n)
			}
		}
	}
	return kids
}

// ProcessTable enumerates every process as (pid, argv) by walking /proc. A pid
// whose cmdline can't be read or is empty (kernel threads) is skipped.
func ProcessTable() ([]Process, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // non-pid entry (e.g. /proc/cpuinfo)
		}
		argv, err := Cmdline(pid)
		if err != nil || len(argv) == 0 {
			continue
		}
		out = append(out, Process{PID: pid, Argv: argv})
	}
	return out, nil
}

// statFields reads /proc/<pid>/stat and returns the whitespace-split fields
// AFTER the "(comm)" field, so index 0 == field 3 (state). The comm field can
// itself contain spaces/parens, so we split on the LAST ')'. Returns
// (nil, false) on any read/parse failure. Shared by Ppid and ProcStart.
func statFields(pid int) ([]string, bool) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil, false
	}
	stat := string(data)
	endComm := strings.LastIndex(stat, ")")
	if endComm < 0 || endComm+2 >= len(stat) {
		return nil, false
	}
	return strings.Fields(stat[endComm+2:]), true
}

// Ppid returns pid's parent pid from /proc/<pid>/stat field 4 (index 1 after
// the comm strip). (0, false) on failure.
func Ppid(pid int) (int, bool) {
	fields, ok := statFields(pid)
	if !ok || len(fields) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ProcStart returns pid's kernel start time (jiffies since boot) from
// /proc/<pid>/stat field 22 (index 19 after the comm strip), as a string token.
// This is the SAME token Claude stores as procStart in the Linux session file,
// so leadsession compares them directly (normalizeFileProcStart is identity on
// Linux).
func ProcStart(pid int) (string, bool) {
	fields, ok := statFields(pid)
	if !ok || len(fields) < 20 {
		return "", false
	}
	return fields[19], true
}
