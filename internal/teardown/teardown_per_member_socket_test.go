package teardown

import (
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// TestTeardownTeam_MixedMode_PerMemberSocket: a single team can hold a mix of
// in-tmux members (Member.Socket == "") and swarm members (Member.Socket ==
// "cc-fleet-swarm-<team>"). With only the team-level TmuxSocket as authority, a
// mixed team mis-targets the kill-pane either to the default server (swallowed
// for swarm panes) or to the private socket (swallowed for in-tmux panes),
// silently leaking the wrong pane.
//
// Assertion: TeardownTeam runs kill-pane with the SOCKET RECORDED ON EACH
// MEMBER. We script a fake tmux that logs argv and assert:
//   - swarm pane gets `-L <sock> kill-pane -t <pane>` (socket-scoped)
//   - in-tmux pane gets `kill-pane -t <pane>` (no -L)
//
// The team-level TmuxSocket() is left as a swarm socket too so the legacy
// fallback path is exercised when one of the members has Socket=="".
func TestTeardownTeam_MixedMode_PerMemberSocket(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	const sock = "cc-fleet-swarm-mix"
	// Build a team with one swarm member (Member.Socket set) and one in-tmux
	// member (Member.Socket empty, no team-level fallback). The legacy
	// TeamConfig.TmuxSocket is also "" — the in-tmux member is genuinely on
	// the default server.
	members := []spawn.Member{
		{Name: "swarm1", AgentID: "swarm1@mix", TmuxPaneID: "%200", AgentType: "general-purpose", Socket: sock},
		{Name: "intmux1", AgentID: "intmux1@mix", TmuxPaneID: "%201", AgentType: "general-purpose"},
	}
	tc := &spawn.TeamConfig{LeadSessionID: "lead-uuid", Members: members, Raw: map[string]any{}}
	if err := spawn.EnsureTeamDir("mix"); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	if err := spawn.WriteTeamConfig("mix", tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}

	res := TeardownTeam("mix")
	if !res.OK {
		t.Fatalf("teardown failed: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}

	calls := readFakeTmuxCalls(t, argsPath)
	// swarm member: kill-pane MUST be socket-scoped.
	if !containsCall(calls, []string{"-L", sock, "kill-pane", "-t", "%200"}) {
		t.Fatalf("missing socket-scoped kill-pane for swarm member; calls=%v", calls)
	}
	// in-tmux member: kill-pane MUST be on the default server (no -L).
	if !containsCall(calls, []string{"kill-pane", "-t", "%201"}) {
		t.Fatalf("missing default-server kill-pane for in-tmux member; calls=%v", calls)
	}
	// kill-server runs once for the swarm socket and NEVER for the default.
	if !containsCall(calls, []string{"-L", sock, "kill-server"}) {
		t.Fatalf("missing socket kill-server after mixed teardown; calls=%v", calls)
	}
	for _, c := range calls {
		// Guard: the default server must never see kill-server.
		if len(c) == 1 && c[0] == "kill-server" {
			t.Fatalf("default-server kill-server seen on mixed-mode teardown; calls=%v", calls)
		}
	}
}

// TestTeardownTeam_LegacyTeamSocketFallback covers the backward-compatibility
// path: an older config written before Member.Socket existed has every member's
// Socket empty but a team-level TmuxSocket. teardown must still route kill-pane
// to the right server using the legacy fallback — otherwise upgrading cc-fleet
// silently breaks pre-existing swarm teams.
func TestTeardownTeam_LegacyTeamSocketFallback(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	const sock = "cc-fleet-swarm-legacy"
	// All members lack the per-member Socket field — simulates an old config.
	seedSwarmTeam(t, "legacy", sock, []spawn.Member{
		{Name: "w1", AgentID: "w1@legacy", TmuxPaneID: "%300", AgentType: "general-purpose"},
		{Name: "w2", AgentID: "w2@legacy", TmuxPaneID: "%301", AgentType: "general-purpose"},
	})

	res := TeardownTeam("legacy")
	if !res.OK {
		t.Fatalf("teardown failed: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	calls := readFakeTmuxCalls(t, argsPath)
	for _, pane := range []string{"%300", "%301"} {
		want := []string{"-L", sock, "kill-pane", "-t", pane}
		if !containsCall(calls, want) {
			t.Fatalf("legacy fallback: missing socket-scoped kill-pane for %s; calls=%v", pane, calls)
		}
	}
}
