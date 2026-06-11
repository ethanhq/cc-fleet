// Package doctor implements the health checks behind `cc-fleet doctor`,
// split into a Core group (every run mode) and an Optional group (live
// teammates only — tmux).
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
	// OAuth credentials missing when the user only ever uses provider profiles.
	// Warns do NOT make DoctorResult.OK false.
	StatusWarn Status = "warn"
	// StatusFail means the check failed and the user almost certainly needs
	// to act. A Core-group fail flips DoctorResult.OK to false; an Optional one
	// does not (see Group).
	StatusFail Status = "fail"
)

// Group classifies a check by which run modes need it. Core checks apply to
// every mode (subagent / workflow / run / teammate); Optional checks matter
// only for live teammates (tmux). Only a Core fail flips DoctorResult.OK.
type Group string

const (
	// GroupCore is needed by every cc-fleet run mode.
	GroupCore Group = "core"
	// GroupOptional is needed only by live teammates (tmux).
	GroupOptional Group = "optional"
)

// CheckResult is the verdict of a single check.
//
// Field JSON tags are part of the public contract `cc-fleet doctor --json`
// emits — the skill dispatches on ID and Status. Don't rename tags without
// updating the skill.
type CheckResult struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Status  Status `json:"status"`
	Group   Group  `json:"group"`
	Detail  string `json:"detail,omitempty"`
	Fixable bool   `json:"fixable,omitempty"`
	FixHint string `json:"fix_hint,omitempty"`
}

// DoctorResult is the full output of RunAll — one envelope around the
// CheckResults plus an aggregate OK boolean. OK is the AND of "status !=
// StatusFail" across the Core-group results only; an Optional (live-teammate)
// failure never flips OK.
type DoctorResult struct {
	OK      bool          `json:"ok"`
	Results []CheckResult `json:"results"`
}
