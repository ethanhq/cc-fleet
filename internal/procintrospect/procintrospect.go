// Package procintrospect provides the small set of process-introspection
// operations cc-fleet performs by reading process state: a process's argv, its
// immediate child pids, and the whole process table.
//
// Each operation is platform-split via build tags so the binary carries exactly
// one implementation per OS:
//
//   - linux   (procintrospect_linux.go)   — reads /proc.
//   - darwin  (procintrospect_darwin.go)  — sysctl kern.procargs2/kern.proc.all
//     for argv and the table; ps(1)/pgrep(1) for ppid, start time, children. No cgo.
//   - windows (procintrospect_windows.go) — GetProcessTimes start tokens +
//     Toolhelp32 parent lookup; argv/table stay unsupported (no PEB read).
//   - other   (procintrospect_other.go)   — degrades to empty/unsupported.
//
// It is the single shared home for the pattern: spawn (rollback reap) and
// teardown (ps / board / hide-show discovery + ghost reap) both compose these
// primitives instead of each open-coding a /proc scan.
//
// All readers are best-effort: a vanished pid, a permission error, or a race
// against process exit yields a nil/empty result rather than a hard failure.
// Where argv is supported (linux, darwin) it is the exact argument vector, not
// a re-split command string.
package procintrospect

// Process is one row of the process table: a pid and its argv.
type Process struct {
	PID  int
	Argv []string
}
