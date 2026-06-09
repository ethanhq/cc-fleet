package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// RunAll runs every check and assembles the DoctorResult.
//
// If fix is true, doctor attempts a conservative auto-repair for the small
// set of issues it knows how to fix safely:
//
//   - Check 2 (~/.claude/profiles/ missing): MkdirAll with mode 0700, then
//     re-run the check to verify.
//
// Other Fixable failures (check 7 skill not installed, check 8 fingerprint
// stale) print fix hints but are NOT auto-repaired:
//   - Skill install belongs to the install machinery.
//   - Fingerprint refresh requires a live native Agent probe; only Claude
//     (via the skill) can spawn that. Doctor surfaces the hint and exits.
//
// Check 1 (settings.json missing) is intentionally NOT fixed — creating it
// would commit policy decisions doctor shouldn't make on the user's behalf.
//
// Results are returned in check-ID order so JSON consumers can index by
// position. OK = AND of every result.Status != StatusFail.
func RunAll(fix bool) DoctorResult {
	checks := []func() CheckResult{
		CheckSettingsJSON,
		CheckProfilesDirWritable,
		CheckTmuxInstalled,
		CheckClaudeBinary,
		CheckAttachedTmux,
		CheckVendorKeys,
		CheckSkillInstalled,
		CheckFingerprint,
		CheckOAuthCredentials,
		CheckPluginVersionMatch,
	}

	results := make([]CheckResult, 0, len(checks))
	for _, fn := range checks {
		r := fn()
		if fix && r.Status == StatusFail && r.Fixable {
			if applied := tryFix(&r); applied {
				r.AppliedFix = true
				// Re-run the same check to record the post-fix state. We
				// only auto-fix check 2, so the only re-runnable path is
				// CheckProfilesDirWritable.
				if r.ID == 2 {
					post := CheckProfilesDirWritable()
					post.AppliedFix = true
					post.Fixable = r.Fixable
					post.FixHint = r.FixHint
					r = post
				}
			}
		}
		r.Group = groupForID(r.ID)
		results = append(results, r)
	}

	// Preserve check-ID order even if someone reorders checks above.
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })

	// OK = no Core check failed. Optional (live-teammate) checks never flip OK,
	// so a tmux-less machine that only uses subagent/workflow/run is healthy.
	ok := true
	for _, r := range results {
		if r.Group != GroupOptional && r.Status == StatusFail {
			ok = false
			break
		}
	}
	return DoctorResult{OK: ok, Results: results}
}

// groupForID classifies a check. Only tmux-related checks (3 installed, 5
// attached) are Optional — everything else is Core.
func groupForID(id int) Group {
	switch id {
	case 3, 5:
		return GroupOptional
	default:
		return GroupCore
	}
}

// tryFix runs the auto-fix action for a single check, returning true if it
// did any work (regardless of whether the work succeeded — AppliedFix means
// "we tried", not "we succeeded"; the re-run check determines success).
//
// Right now only check 2 has a safe auto-fix: mkdir the missing
// ~/.claude/profiles dir with mode 0700.
func tryFix(r *CheckResult) bool {
	switch r.ID {
	case 2:
		cdir, err := claudeDir()
		if err != nil {
			// Can't fix without HOME; leave r as-is.
			return false
		}
		path := filepath.Join(cdir, "profiles")
		// Best-effort mkdir; ignore errors — the post-fix re-run will
		// surface them through the standard check result.
		_ = os.MkdirAll(path, 0o700)
		return true
	}
	return false
}

// statusSymbol returns a single-char glyph for pretty rendering. Kept here
// (vs in the cmd package) so tests can reuse the same legend if needed.
func statusSymbol(s Status) string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusWarn:
		return "WARN"
	case StatusFail:
		return "FAIL"
	default:
		return "?"
	}
}

// FormatLine renders one result as the human-friendly line shown by `cc-fleet
// doctor` (pretty mode). The cmd package wraps these with the surrounding
// header / summary; the rendering itself lives here so it stays alongside the
// result type.
func FormatLine(total int, r CheckResult) string {
	line := fmt.Sprintf("[%d/%d] %s  %s", r.ID, total, statusSymbol(r.Status), r.Title)
	if r.Detail != "" {
		line += " — " + r.Detail
	}
	if r.AppliedFix {
		line += " (auto-fix applied)"
	} else if r.Status == StatusFail && r.FixHint != "" {
		line += "\n        hint: " + r.FixHint
	}
	return line
}
