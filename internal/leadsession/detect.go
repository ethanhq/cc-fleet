// Package leadsession detects the parent Claude Code session for commands that
// are launched from a Claude Bash tool but do not otherwise have a team context.
package leadsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/homedir"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

const maxAncestorDepth = 64

var procRoot = "/proc"

type sessionFile struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	ProcStart string `json:"procStart,omitempty"`
}

// Detect returns the current parent Claude session id, if cc-fleet appears to
// be running under a top-level Claude Code process. Best-effort: failure to
// identify a session returns "".
func Detect() string {
	return DetectFromPID(os.Getppid())
}

// DetectFromPID walks upward from pid and returns the first live Claude session
// registry entry it can validate. Exported for tests.
func DetectFromPID(pid int) string {
	id, _ := walk(pid)
	return id
}

// DetectPIDWithStart walks upward from the current parent PID and returns the
// first ancestor PID whose Claude session registry entry validates (same
// fail-closed PID-reuse check Detect() uses), plus that PID's validated /proc
// start time. Returns (0, "") when no validated ancestor exists. Used by
// spawn-time permission inheritance to read the lead's cmdline.
//
// The caller reads the lead's cmdline by bare PID AFTER this detect; the PID can
// be recycled in between (TOCTOU). Returning the detect-time procStart lets the
// caller re-confirm (RevalidateProcStart) the PID still names the same process
// before trusting its cmdline, else fail safe (frozen-template, never mis-inherit).
func DetectPIDWithStart() (pid int, procStartTime string) {
	id, p := walk(os.Getppid())
	if id == "" || p == 0 {
		return 0, ""
	}
	st, ok := procStart(p)
	if !ok {
		return 0, ""
	}
	return p, st
}

// RevalidateProcStart reports whether pid's current start time still equals want
// (captured at detect time). A false result means the PID was recycled (or the
// process is gone) between detection and this call. When start time is
// unavailable it returns false — the safe answer: callers fall back rather than
// trust an unverifiable PID.
func RevalidateProcStart(pid int, want string) bool {
	if pid <= 0 || want == "" {
		return false
	}
	st, ok := procStart(pid)
	return ok && st == want
}

// walk is the shared ancestor walk used by Detect/DetectFromPID and DetectPIDWithStart.
// It returns the (sessionID, pid) of the first ancestor whose session file
// validates. When nothing validates it returns ("", 0). The walk stops at
// init, on cycles, or after maxAncestorDepth steps.
func walk(pid int) (string, int) {
	seen := map[int]struct{}{}
	for depth := 0; pid > 1 && depth < maxAncestorDepth; depth++ {
		if _, ok := seen[pid]; ok {
			return "", 0
		}
		seen[pid] = struct{}{}

		if id := sessionIDForPID(pid); id != "" {
			return id, pid
		}
		next, ok := parentPID(pid)
		if !ok {
			return "", 0
		}
		pid = next
	}
	return "", 0
}

func sessionIDForPID(pid int) string {
	sf, ok := readSessionFile(pid)
	if !ok || sf.SessionID == "" {
		return ""
	}
	if sf.PID != 0 && sf.PID != pid {
		return ""
	}
	// Fail closed: without procStart we cannot distinguish a still-live Claude
	// process from a recycled PID holding an old session file.
	if sf.ProcStart == "" {
		return ""
	}
	if st, ok := procStart(pid); !ok || st != normalizeFileProcStart(sf.ProcStart) {
		return ""
	}
	return sf.SessionID
}

// normalizeFileProcStart maps the procStart value stored in a Claude session
// file into the SAME token space procStart(pid) returns, so the PID-reuse guard
// can compare them.
//
// Linux: the file stores the kernel start-time jiffies token (identical to
// /proc/<pid>/stat field 22), returned unchanged.
//
// macOS: the file stores a UTC date string ("Mon Jan _2 15:04:05 2006") while
// procStart(pid) returns Unix epoch seconds (from `ps -o lstart=`, local time).
// We parse the file's UTC date to epoch seconds so both sides are the same
// instant in the same representation. A parse failure returns the raw value,
// which cannot equal the epoch token → the guard fails closed.
func normalizeFileProcStart(fileVal string) string {
	if runtime.GOOS != "darwin" {
		return fileVal
	}
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", fileVal)
	if err != nil {
		return fileVal
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func readSessionFile(pid int) (sessionFile, bool) {
	var sf sessionFile
	root := claudeConfigDir()
	if root == "" {
		return sf, false
	}
	data, err := os.ReadFile(filepath.Join(root, "sessions", strconv.Itoa(pid)+".json"))
	if err != nil {
		return sf, false
	}
	if err := json.Unmarshal(data, &sf); err != nil {
		return sf, false
	}
	return sf, true
}

func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, err := homedir.Home()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func parentPID(pid int) (int, bool) {
	switch runtime.GOOS {
	case "linux":
		fields, ok := procStatFields(pid)
		if !ok || len(fields) < 2 {
			return 0, false
		}
		ppid, err := strconv.Atoi(fields[1])
		return ppid, err == nil
	case "darwin":
		// macOS has no /proc; use `ps -o ppid=`.
		return procintrospect.Ppid(pid)
	case "windows":
		// Windows has no /proc; use the Toolhelp32 snapshot.
		return procintrospect.Ppid(pid)
	}
	return 0, false
}

func procStart(pid int) (string, bool) {
	switch runtime.GOOS {
	case "linux":
		fields, ok := procStatFields(pid)
		if !ok || len(fields) < 20 {
			return "", false
		}
		return fields[19], true
	case "darwin":
		// epoch-seconds token from `ps -o lstart=`; compared against the file's
		// UTC date via normalizeFileProcStart.
		return procintrospect.ProcStart(pid)
	case "windows":
		// decimal-nanos creation token from GetProcessTimes; platform-local
		// equality only, compared against whatever the session file stores.
		return procintrospect.ProcStart(pid)
	}
	return "", false
}

func procStatFields(pid int) ([]string, bool) {
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
