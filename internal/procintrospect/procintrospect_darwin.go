//go:build darwin

package procintrospect

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// execCommand is a seam so darwin tests can stub ps/pgrep without spawning real
// processes. Production wiring is os/exec.Command.
var execCommand = exec.Command

var errBadProcargs = errors.New("procintrospect: malformed kern.procargs2")

// Cmdline returns pid's exact argv from the kern.procargs2 sysctl, so arguments
// containing spaces, quotes or nothing at all come back unchanged.
//
// A gone pid or one we may not inspect (EPERM) yields (nil, err), the same
// outcome the Linux reader gives for a missing /proc/<pid>/cmdline.
func Cmdline(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcargs2(buf)
}

// parseProcargs2 decodes the kern.procargs2 layout: a little-endian int32 argc
// (both darwin targets are little-endian),
// the NUL-terminated exec path plus NUL padding, then argc NUL-terminated
// argv strings (the environment follows and is ignored).
func parseProcargs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, errBadProcargs
	}
	argc := int(int32(binary.LittleEndian.Uint32(buf)))
	rest := buf[4:]
	end := bytes.IndexByte(rest, 0)
	if argc < 0 || end < 0 {
		return nil, errBadProcargs
	}
	rest = bytes.TrimLeft(rest[end:], "\x00")
	argv := make([]string, 0, argc)
	for len(argv) < argc {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, errBadProcargs
		}
		argv = append(argv, string(rest[:end]))
		rest = rest[end+1:]
	}
	return argv, nil
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

// ProcessTable enumerates every process as (pid, argv): one kern.proc.all
// sysctl for the pids, then Cmdline per pid. Processes whose argv cannot be
// read (exited, other users' EPERM, kernel_task) are skipped.
func ProcessTable() ([]Process, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var procs []Process
	for _, kp := range kps {
		pid := int(kp.Proc.P_pid)
		argv, err := Cmdline(pid)
		if err != nil || len(argv) == 0 {
			continue
		}
		procs = append(procs, Process{PID: pid, Argv: argv})
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
// for SAME-PLATFORM equality (the PID-reuse guards) and is compared against the
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

// StartFollowsClockSteps reports whether StartUnixMilli moves with wall-clock
// steps made after the process started: lstart is the recorded fork time.
const StartFollowsClockSteps = false

// StartUnixMilli converts a ProcStart token (Unix seconds) to Unix
// milliseconds. lstart is whole seconds, so the result can be up to a second
// early.
func StartUnixMilli(token string) (int64, bool) {
	sec, err := strconv.ParseInt(token, 10, 64)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return sec * 1000, true
}
