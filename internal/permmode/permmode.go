// Package permmode is the shared permission-mode vocabulary for cc-fleet's
// claude launchers (spawn teammates and run sessions): the --permission-mode
// enum, its validation, and the enum→CLI-flag mapping. One source so spawn and
// run can't drift on what a mode means or maps to.
package permmode

// The accepted --permission-mode values (mirrors native Claude Code).
const (
	Default           = "default"
	AcceptEdits       = "acceptEdits"
	Plan              = "plan"
	Auto              = "auto"
	BypassPermissions = "bypassPermissions"
)

// Modes is the accepted set, in a stable order for error messages.
var Modes = []string{Default, AcceptEdits, Plan, Auto, BypassPermissions}

// IsValid reports whether mode is an accepted --permission-mode value.
func IsValid(mode string) bool {
	for _, m := range Modes {
		if mode == m {
			return true
		}
	}
	return false
}

// ToFlags is the INHERITANCE mapping (spawn): bypassPermissions →
// --dangerously-skip-permissions; acceptEdits / auto → --permission-mode <mode>;
// default / plan / unknown → no flag. plan and default collapse to no flag on
// purpose — a lead in plan/default mode must NOT propagate a permission flag to
// the teammate. For a user's EXPLICIT choice (e.g. `run --permission-mode plan`)
// use ExplicitFlags, which forwards every mode faithfully.
func ToFlags(mode string) []string {
	switch mode {
	case BypassPermissions:
		return []string{"--dangerously-skip-permissions"}
	case AcceptEdits:
		return []string{"--permission-mode", AcceptEdits}
	case Auto:
		return []string{"--permission-mode", Auto}
	}
	return nil
}

// ExplicitFlags is the faithful mapping for a user's explicitly chosen mode (run):
// bypassPermissions → --dangerously-skip-permissions; every other valid mode
// (default / acceptEdits / plan / auto) → --permission-mode <mode>; "" → no flag.
// Unlike ToFlags it does NOT collapse plan/default, so `run --permission-mode plan`
// actually reaches claude. Callers validate the mode (IsValid) first.
func ExplicitFlags(mode string) []string {
	switch mode {
	case "":
		return nil
	case BypassPermissions:
		return []string{"--dangerously-skip-permissions"}
	default:
		return []string{"--permission-mode", mode}
	}
}
