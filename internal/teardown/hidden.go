package teardown

import (
	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// AnnotateHidden fills the Hidden flag on each teammate from its team's
// config.json — the data source the hide/show flow (panevis) persists into
// Member.Hidden. This is how the status board surfaces which live teammates
// currently have their pane parked in the detached hidden session.
//
// Each team's config is read at most once (grouped by team). Best-effort: a
// team whose config can't be read leaves its teammates' Hidden=false rather
// than failing the whole listing — exactly like AnnotateHealth degrades an
// uncapturable pane to statusUnknown. Mutates and returns the same slice.
func AnnotateHidden(teammates []Teammate) []Teammate {
	// nil entry = a team whose config we already tried and failed to read;
	// remembering it avoids re-loading per teammate of that team.
	byTeam := map[string]*spawn.TeamConfig{}
	for i := range teammates {
		team := teammates[i].Team
		if team == "" {
			continue
		}
		tc, seen := byTeam[team]
		if !seen {
			loaded, err := spawn.LoadTeamConfig(team)
			if err != nil {
				byTeam[team] = nil
				continue
			}
			tc = loaded
			byTeam[team] = tc
		}
		if tc == nil {
			continue
		}
		for _, m := range tc.Members {
			if m.Name == teammates[i].Name {
				teammates[i].Hidden = m.Hidden
				break
			}
		}
	}
	return teammates
}

// AnnotateLeadSession fills LeadSessionID on each teammate from its team's
// config.json. Spawn persists per-member LeadSessionID AND the team-level
// TeamConfig.LeadSessionID; this annotation uses the per-member value when set
// so a re-used team with a different caller groups members under their actual
// parent Claude session, falling back to the team-level field when a member's
// slot is empty — preserves behavior for older configs written before
// per-member LeadSessionID existed.
//
// Best-effort and one read per team, matching AnnotateHidden.
func AnnotateLeadSession(teammates []Teammate) []Teammate {
	byTeam := map[string]*spawn.TeamConfig{}
	for i := range teammates {
		team := teammates[i].Team
		if team == "" {
			continue
		}
		tc, seen := byTeam[team]
		if !seen {
			loaded, err := spawn.LoadTeamConfig(team)
			if err != nil {
				byTeam[team] = nil
				continue
			}
			tc = loaded
			byTeam[team] = tc
		}
		if tc == nil {
			continue
		}
		// Default to the team-level fallback; the per-member walk overrides
		// when the member record carries an explicit LeadSessionID.
		teammates[i].LeadSessionID = tc.LeadSessionID
		for _, m := range tc.Members {
			if m.Name == teammates[i].Name {
				teammates[i].SpawnTime = m.JoinedAt
				if m.LeadSessionID != "" {
					teammates[i].LeadSessionID = m.LeadSessionID
				}
				break
			}
		}
	}
	return teammates
}
