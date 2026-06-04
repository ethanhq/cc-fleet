//go:build !windows

package teardown

import (
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// TestAnnotateLeadSession_PerMemberPriorityAndFallback: a re-used team can hold
// members spawned by DIFFERENT Claude parent sessions. Copying the team-level
// TeamConfig.LeadSessionID to every teammate row would mis-group the board's
// session view, so LeadSessionID is recorded per-member and the annotator
// PREFERS the per-member value, falling back to team-level only when the
// member's slot is empty (backward compatibility with older configs).
//
// Setup: two members with distinct LeadSessionIDs ("session-B" and "") and a
// team-level LeadSessionID = "session-A". Expected:
//   - alice → "session-B" (per-member wins)
//   - bob   → "session-A" (per-member empty → fallback to team-level)
func TestAnnotateLeadSession_PerMemberPriorityAndFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// seedTeam writes LeadSessionID="lead-uuid" by default — override by
	// constructing the TeamConfig manually so we can choose "session-A".
	const teamLevel = "session-A"
	const aliceLead = "session-B"
	members := []spawn.Member{
		{Name: "alice", AgentID: "alice@team", TmuxPaneID: "%10", AgentType: "general-purpose", JoinedAt: 1000, LeadSessionID: aliceLead},
		{Name: "bob", AgentID: "bob@team", TmuxPaneID: "%11", AgentType: "general-purpose", JoinedAt: 2000}, // LeadSessionID = "" → fallback
	}
	tc := &spawn.TeamConfig{LeadSessionID: teamLevel, Members: members, Raw: map[string]any{}}
	if err := spawn.EnsureTeamDir("team"); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	if err := spawn.WriteTeamConfig("team", tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}

	out := AnnotateLeadSession([]Teammate{
		{Name: "alice", Team: "team"},
		{Name: "bob", Team: "team"},
	})

	if out[0].LeadSessionID != aliceLead {
		t.Fatalf("alice: LeadSessionID = %q, want per-member %q", out[0].LeadSessionID, aliceLead)
	}
	if out[1].LeadSessionID != teamLevel {
		t.Fatalf("bob: LeadSessionID = %q, want team-level fallback %q", out[1].LeadSessionID, teamLevel)
	}
	// JoinedAt → SpawnTime mapping kept (legacy contract).
	if out[0].SpawnTime != 1000 || out[1].SpawnTime != 2000 {
		t.Fatalf("SpawnTime mapping broken: alice=%d bob=%d, want (1000,2000)", out[0].SpawnTime, out[1].SpawnTime)
	}
}

// TestAnnotateLeadSession_LegacyConfigStillWorks covers the backward-
// compatibility path: a config written before per-member LeadSessionID
// existed (every Member.LeadSessionID == "") must keep working — every
// member inherits the team-level value.
func TestAnnotateLeadSession_LegacyConfigStillWorks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTeam(t, "legacy", []spawn.Member{
		{Name: "alice", AgentID: "alice@legacy", TmuxPaneID: "%30", AgentType: "general-purpose", JoinedAt: 100},
		{Name: "bob", AgentID: "bob@legacy", TmuxPaneID: "%31", AgentType: "general-purpose", JoinedAt: 200},
	})
	// seedTeam writes LeadSessionID = "lead-uuid" team-level.
	out := AnnotateLeadSession([]Teammate{
		{Name: "alice", Team: "legacy"},
		{Name: "bob", Team: "legacy"},
	})
	for i, r := range out {
		if r.LeadSessionID != "lead-uuid" {
			t.Fatalf("row %d LeadSessionID = %q, want lead-uuid (legacy fallback)", i, r.LeadSessionID)
		}
	}
}
