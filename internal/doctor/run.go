package doctor

import (
	"fmt"
	"sort"
)

// RunAll runs every check and assembles the DoctorResult.
//
// Doctor never repairs anything — Fixable failures (check 7 skill not
// installed, check 8 fingerprint stale) carry fix hints for the user or the
// skill to act on:
//   - Skill install belongs to the install machinery.
//   - Fingerprint refresh requires a live native Agent probe; only Claude
//     (via the skill) can spawn that. Doctor surfaces the hint and exits.
//   - settings.json (check 1) is the user's — creating it would commit policy
//     decisions doctor shouldn't make on their behalf.
//
// Results are returned in check-ID order so JSON consumers can index by
// position. OK is true unless a Core-group check failed; an Optional
// (live-teammate) failure never flips it.
func RunAll() DoctorResult {
	checks := []func() CheckResult{
		CheckSettingsJSON,
		CheckProfilesDirWritable,
		CheckTmuxInstalled,
		CheckClaudeBinary,
		CheckAttachedTmux,
		CheckProviderKeys,
		CheckSkillInstalled,
		CheckFingerprint,
		CheckOAuthCredentials,
		CheckPluginVersionMatch,
	}

	results := make([]CheckResult, 0, len(checks))
	for _, fn := range checks {
		r := fn()
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
	if r.Status != StatusOK && r.FixHint != "" {
		line += "\n        hint: " + r.FixHint
	}
	return line
}
