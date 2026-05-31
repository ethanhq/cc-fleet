// Package doctor implements the nine health checks behind `cc-fleet doctor`.
//
// Each check is an independent function in checks.go that returns a
// CheckResult value — none of them panic, even on grossly broken systems.
// run.go composes them into the final DoctorResult that the cmd surfaces as
// JSON (skill-consumable) or pretty (human-consumable) output.
//
// The check IDs and titles are a stable contract; reorder only by coordinating
// with the skill that dispatches on them.
package doctor

// Status is the outcome class of a single check. Three values cover what we
// need: ok (green), warn (informational fail — leaves OK=true overall), and
// fail (red — flips OK to false).
type Status string

const (
	// StatusOK means the check passed.
	StatusOK Status = "ok"
	// StatusWarn means the check failed in a way that's informational — e.g.
	// OAuth credentials missing when the user only ever uses vendor profiles.
	// Warns do NOT make DoctorResult.OK false.
	StatusWarn Status = "warn"
	// StatusFail means the check failed and the user almost certainly needs
	// to act. Any single fail flips DoctorResult.OK to false.
	StatusFail Status = "fail"
)

// CheckResult is the verdict of a single check.
//
// Field JSON tags are part of the public contract `cc-fleet doctor --json`
// emits — the skill dispatches on ID and Status. Don't rename tags without
// updating the skill.
type CheckResult struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Status     Status `json:"status"`
	Detail     string `json:"detail,omitempty"`
	Fixable    bool   `json:"fixable,omitempty"`
	FixHint    string `json:"fix_hint,omitempty"`
	AppliedFix bool   `json:"applied_fix,omitempty"`
}

// DoctorResult is the full output of RunAll — one envelope around the nine
// CheckResults plus an aggregate OK boolean. OK is the AND of "status !=
// StatusFail" across all results.
type DoctorResult struct {
	OK      bool          `json:"ok"`
	Results []CheckResult `json:"results"`
}
