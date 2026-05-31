package spawn

import (
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// Permission-mode enum values accepted by --permission-mode. The CLI validates
// a manual override against this set; inheritPermissionFlags emits the matching
// argv pair for `manual` / `lead-flag` sources.
const (
	PermModeDefault           = "default"
	PermModeAcceptEdits       = "acceptEdits"
	PermModePlan              = "plan"
	PermModeAuto              = "auto"
	PermModeBypassPermissions = "bypassPermissions"
)

// detectLeadPID, readLeadCmdline, and revalidateLead are swappable seams so
// spawn's own tests drive inheritPermissionFlags without a live lead process or
// a real /proc. Production wires them to leadsession.DetectPIDWithStart, the
// procintrospect-backed readLeadCmdline, and leadsession.RevalidateProcStart;
// the inherit_test.go helper swaps them per-test with restore.
//
// detectLeadPID returns the lead's validated start time alongside its PID so
// revalidateLead can re-confirm the PID still names the same process AFTER
// readLeadCmdline — closing the detect→read PID-reuse window.
//
// readLeadCmdline routes through internal/procintrospect (the cross-platform
// argv outlet — Linux /proc, darwin `ps`) rather than reading /proc directly,
// which macOS lacks. Permission flags carry no whitespace, so darwin's
// space-split argv is exact for this use.
var (
	detectLeadPID   = leadsession.DetectPIDWithStart
	readLeadCmdline = readLeadCmdlineProcintrospect
	revalidateLead  = leadsession.RevalidateProcStart
)

// readLeadCmdlineProcintrospect reads pid's argv via procintrospect.Cmdline and
// adapts it to the (argv, ok) shape inheritPermissionFlags expects: ok=false on
// a read error (→ frozen-template fallback), ok=true otherwise (an empty argv
// still means "lead read succeeded, no permission flag" → lead-default). It does
// NOT read /proc directly; procintrospect is the single cross-platform argv
// outlet (Linux /proc, darwin `ps`).
func readLeadCmdlineProcintrospect(pid int) ([]string, bool) {
	argv, err := procintrospect.Cmdline(pid)
	if err != nil {
		return nil, false
	}
	return argv, true
}

// hasBareFlag reports whether args contains a token exactly equal to flag.
// Used to detect `--dangerously-skip-permissions` in both the lead's
// /proc/<pid>/cmdline and the fingerprint's expanded argv (for stripping).
func hasBareFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// flagValue returns the value following a `--flag value` pair. Returns
// ("", false) when flag is absent or appears as the trailing token (no value
// follows). Only the space-separated shape is handled; cc-fleet never produces
// `--flag=value` for these flags.
func flagValue(args []string, flag string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// permModeToFlags maps a permission-mode enum to the CLI flag pair the spawn
// should carry. Returns nil for default/plan (no flag — plan is intentionally
// not inherited) and for unknown modes (the CLI rejects those before here).
// Used by both the manual-override and lead-cmdline branches of
// inheritPermissionFlags so they map identically.
func permModeToFlags(mode string) []string {
	switch mode {
	case PermModeBypassPermissions:
		return []string{"--dangerously-skip-permissions"}
	case PermModeAcceptEdits:
		return []string{"--permission-mode", PermModeAcceptEdits}
	case PermModeAuto:
		return []string{"--permission-mode", PermModeAuto}
	case PermModeDefault, PermModePlan:
		return nil
	}
	return nil
}

// inheritPermissionFlags computes the permission-related CLI flags the new
// teammate spawn should carry, and the source label that explains where the
// decision came from. Source is one of "manual" / "lead-flag" /
// "lead-default" / "frozen-template".
//
// Decision order:
//  1. manualOverride non-empty → use it, source=manual (CLI guarantees
//     manualOverride is a valid mode by this point).
//  2. leadsession.DetectPIDWithStart() == 0 → source=frozen-template (fallback
//     β: no validated lead, sit on fingerprint template). Covers macOS /
//     out-of-tmux / external-shell launches.
//  3. readLeadCmdline(leadPID) fails → source=frozen-template (same fallback β:
//     cmdline unreadable, can't introspect lead).
//  4. After reading the cmdline, re-validate the lead PID's start time still
//     equals the detect-time value. A mismatch means the lead exited and its
//     PID was recycled in the detect→read window — we would otherwise inherit
//     flags from an unrelated process. On mismatch → source=frozen-template
//     (fail safe: NEVER mis-inherit).
//  5. Lead cmdline parsed AND revalidated → source=lead-flag for the
//     explicit-mode cases, lead-default for plan / default / no permission flag.
//
// Caller (buildSpawnCommand) strips the fingerprint-template permission flags
// before appending these and appends `inherited` (nil on frozen-template →
// claude's safe interactive default).
func inheritPermissionFlags(manualOverride string) ([]string, string) {
	if manualOverride != "" {
		return permModeToFlags(manualOverride), "manual"
	}
	leadPID, leadStart := detectLeadPID()
	if leadPID == 0 {
		return nil, "frozen-template"
	}
	argv, ok := readLeadCmdline(leadPID)
	if !ok {
		return nil, "frozen-template"
	}
	// Confirm the PID still names the SAME process we validated at detect time
	// before trusting the cmdline we just read. If the start time changed (PID
	// reuse) or the process is gone, fail safe.
	if !revalidateLead(leadPID, leadStart) {
		return nil, "frozen-template"
	}
	if hasBareFlag(argv, "--dangerously-skip-permissions") {
		return []string{"--dangerously-skip-permissions"}, "lead-flag"
	}
	mode, ok := flagValue(argv, "--permission-mode")
	if !ok {
		return nil, "lead-default"
	}
	switch mode {
	case PermModeBypassPermissions:
		return []string{"--dangerously-skip-permissions"}, "lead-flag"
	case PermModeAcceptEdits:
		return []string{"--permission-mode", PermModeAcceptEdits}, "lead-flag"
	case PermModeAuto:
		return []string{"--permission-mode", PermModeAuto}, "lead-flag"
	}
	// plan / default / unknown all collapse to lead-default (plan must NOT
	// inherit bypass).
	return nil, "lead-default"
}

// stripPermissionFlags returns args with any --dangerously-skip-permissions
// bare flag and any --permission-mode <value> pair removed. Used to clean the
// fingerprint's expanded argv before appending the inherited replacement, so a
// captured permission flag never survives into a spawn.
func stripPermissionFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--dangerously-skip-permissions" {
			continue
		}
		if args[i] == "--permission-mode" && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
