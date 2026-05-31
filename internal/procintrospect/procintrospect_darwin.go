//go:build darwin

package procintrospect

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// execCommand is a seam so darwin tests can stub ps/pgrep without spawning real
// processes. Production wiring is os/exec.Command.
var execCommand = exec.Command

// Cmdline returns pid's argv via `ps -p <pid> -ww -o command=`.
//
// macOS has no /proc and exposes no NUL-delimited argv to userland without cgo,
// so the command string is space-split. An argument that itself contains a
// space (rare — e.g. a --settings path under a HOME that has a space) cannot be
// perfectly reconstructed, but this is sufficient for every cc-fleet marker
// (--agent-id <name>@<team>, --settings <vendor>.json, --model <id>), none of
// which contain whitespace. `-ww` disables ps's column truncation so a long
// teammate command line (binary path + a dozen flags) survives intact.
//
// A gone pid makes ps exit non-zero → (nil, err), the same outcome the Linux
// reader gives for a missing /proc/<pid>/cmdline.
func Cmdline(pid int) ([]string, error) {
	out, err := execCommand("ps", "-p", strconv.Itoa(pid), "-ww", "-o", "command=").Output()
	if err != nil {
		return nil, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, nil
	}
	return strings.Fields(line), nil
}

// Children returns pid's immediate children via `pgrep -P <pid>` — POSIX and
// cross-user-safe (teammates are all the current user's processes). pgrep exits
// 1 with no output when nothing matches, which Output() surfaces as an error;
// that simply yields an empty slice (no children), not a failure.
func Children(pid int) []int {
	out, err := execCommand("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var kids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			kids = append(kids, n)
		}
	}
	return kids
}

// ProcessTable enumerates every process as (pid, argv) with ONE
// `ps -axww -o pid=,command=` (not N per-pid execs). Each line's first
// whitespace-separated field is the pid; the remainder is the space-split argv
// (same whitespace caveat as Cmdline). -axww: all users' processes, no
// truncation.
func ProcessTable() ([]Process, error) {
	out, err := execCommand("ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var procs []Process
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		procs = append(procs, Process{PID: pid, Argv: fields[1:]})
	}
	return procs, nil
}

// Ppid returns pid's parent pid via `ps -o ppid= -p <pid>`. A gone pid or
// unparseable output yields (0, false). Used by leadsession's darwin ancestor
// walk.
func Ppid(pid int) (int, bool) {
	out, err := execCommand("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// procStartLayout is ps(1)'s `lstart` rendering — "Sat May 30 11:33:22 2026" —
// in Go reference-time form. `%e` (day) is space-padded, matching Go's "_2".
const procStartLayout = "Mon Jan _2 15:04:05 2006"

// ProcStart returns pid's start time as a Unix-epoch-seconds STRING.
// macOS exposes start time only as a LOCAL date string via `ps -o lstart=`, so we
// parse it in the local zone and emit epoch seconds — a stable, zone-independent
// token. LC_ALL=C forces the English ps format regardless of the user's locale.
//
// NOTE the token is epoch on darwin but jiffies on linux: it is only meaningful
// for SAME-PLATFORM equality (RevalidateProcStart) and is compared against the
// session file's procStart only after leadsession.normalizeFileProcStart maps the
// file's UTC date string into this same epoch space.
func ProcStart(pid int) (string, bool) {
	cmd := execCommand("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "", false
	}
	t, err := time.ParseInLocation(procStartLayout, s, time.Local)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(t.Unix(), 10), true
}
