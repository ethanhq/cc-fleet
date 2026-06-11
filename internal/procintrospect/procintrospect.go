// Package procintrospect provides the small set of process-introspection
// operations cc-fleet performs by reading process state: a process's argv, its
// immediate child pids, and the whole process table.
//
// Each operation is platform-split via build tags so the binary carries exactly
// one implementation per OS:
//
//   - linux   (procintrospect_linux.go)   — reads /proc.
//   - darwin  (procintrospect_darwin.go)  — shells out to ps(1)/pgrep(1). No cgo.
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
// The marker cc-fleet matches on (--agent-id <name>@<team>, --settings
// <provider>.json) never contains whitespace, so the darwin space-split argv is
// sufficient for every cc-fleet use even though it cannot perfectly recover an
// argument that itself contains a space (see Cmdline's darwin doc).
package procintrospect

// Process is one row of the process table: a pid and its argv.
type Process struct {
	PID  int
	Argv []string
}
