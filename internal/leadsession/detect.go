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

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

const maxAncestorDepth = 64

// ppidFn and procStartFn are seams so tests can fake the process tree.
var (
	ppidFn      = procintrospect.Ppid
	procStartFn = procintrospect.ProcStart
)

// Detect returns the current parent Claude session id, if cc-fleet appears to
// be running under a top-level Claude Code process. Best-effort: failure to
// identify a session returns "".
func Detect() string {
	return DetectFromPID(os.Getppid())
}

// DetectFromPID walks upward from pid and returns the first live Claude session
// registry entry it can validate. Exported for tests.
func DetectFromPID(pid int) string {
	s, _ := walk(pid)
	return s.SessionID
}

// DetectSession walks upward from the parent process and returns the first
// ancestor's validated session registry entry.
func DetectSession() (Session, bool) {
	return walk(os.Getppid())
}

// LiveSessions returns every session registry entry that passes SessionFile, so
// stale files whose pid has since been reused are dropped.
func LiveSessions() []Session {
	dir := claudepaths.Sessions()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var live []Session
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		if s, ok := SessionFile(pid); ok {
			live = append(live, s)
		}
	}
	return live
}

// RegisteredSessions returns every session registry entry, whether or not its
// process is still alive.
func RegisteredSessions() []Session {
	dir := claudepaths.Sessions()
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []Session
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		if s, ok := readSessionFile(pid); ok && s.SessionID != "" {
			all = append(all, s)
		}
	}
	return all
}

// BySessionID returns the live session whose SessionID is exactly id.
func BySessionID(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	for _, s := range LiveSessions() {
		if s.SessionID == id {
			return s, true
		}
	}
	return Session{}, false
}

// CodexThread returns a launcher id derived from the Codex shell environment, or
// "" when cc-fleet was not launched by Codex. Codex exports CODEX_THREAD_ID into
// the env of the shell commands it runs, so a job launched from a Codex session —
// which has no Claude session for Detect to find — can still be attributed and
// grouped on the board instead of collapsing into "(no session)". The "codex:"
// prefix namespaces the value away from Claude session ids (UUIDs carry no ':').
// Consult this only AFTER Detect returns "" so a real Claude session always wins.
func CodexThread() string {
	thread := sanitizeID(strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")))
	if thread == "" {
		return ""
	}
	return "codex:" + thread
}

// sanitizeID strips control bytes and caps length so a launcher-derived id is
// safe as a board grouping key and rendered title.
func sanitizeID(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 128 {
		s = s[:128]
	}
	return s
}

// walk is the shared ancestor walk behind Detect and DetectSession. It returns
// the first ancestor whose session file validates. The walk stops at init, on cycles, or after maxAncestorDepth steps.
func walk(pid int) (Session, bool) {
	seen := map[int]struct{}{}
	for depth := 0; pid > 1 && depth < maxAncestorDepth; depth++ {
		if _, ok := seen[pid]; ok {
			return Session{}, false
		}
		seen[pid] = struct{}{}

		if s, ok := SessionFile(pid); ok {
			return s, true
		}
		next, ok := ppidFn(pid)
		if !ok {
			return Session{}, false
		}
		pid = next
	}
	return Session{}, false
}

// SessionFile reads sessions/<pid>.json and returns it only when the file
// exists, pid is alive, and the file's procStart matches pid's start time.
func SessionFile(pid int) (Session, bool) {
	s, ok := readSessionFile(pid)
	if !ok || s.SessionID == "" {
		return Session{}, false
	}
	if s.PID != 0 && s.PID != pid {
		return Session{}, false
	}
	// Fail closed: without procStart we cannot distinguish a still-live Claude
	// process from a recycled PID holding an old session file.
	if s.ProcStart == "" {
		return Session{}, false
	}
	if st, ok := procStartFn(pid); !ok || st != normalizeFileProcStart(s.ProcStart) {
		return Session{}, false
	}
	s.PID = pid
	return s, true
}

// normalizeFileProcStart maps the procStart value stored in a Claude session
// file into the SAME token space procStartFn(pid) returns, so the PID-reuse
// guard can compare them.
//
// Linux: the file stores the kernel start-time jiffies token (identical to
// /proc/<pid>/stat field 22), returned unchanged.
//
// macOS: the file stores a UTC date string ("Mon Jan _2 15:04:05 2006") while
// procStartFn(pid) returns Unix epoch seconds (from `ps -o lstart=`, local time).
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

// ProcStartMs returns when s's process started, in Unix milliseconds, from the
// session file's procStart (whole seconds on macOS, so up to a second early).
func ProcStartMs(s Session) (int64, bool) {
	return procintrospect.StartUnixMilli(normalizeFileProcStart(s.ProcStart))
}

func readSessionFile(pid int) (Session, bool) {
	var s Session
	dir := claudepaths.Sessions()
	if dir == "" {
		return s, false
	}
	data, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)+".json"))
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, false
	}
	return s, true
}
