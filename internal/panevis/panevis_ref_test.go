package panevis

import (
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// seedSwarmTeam writes a team whose member lives on the private swarm socket
// cc-fleet-swarm-<team> (both the per-member Member.Socket and the team-level
// tmuxSocket marker are set, mirroring a real swarm spawn). Used to prove
// socket routing.
func seedSwarmTeam(t *testing.T, team, name, pane string) {
	t.Helper()
	if err := spawn.EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	socket := spawn.SwarmSocketName(team)
	m := mkMember(name, pane, false, "")
	m.Socket = socket
	tc := &spawn.TeamConfig{LeadSessionID: "lead", Members: []spawn.Member{m}, Raw: map[string]any{}}
	tc.SetTmuxSocket(socket)
	if err := spawn.WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
}

// hasLFlag reports whether any recorded tmux call carried "-L <socket>".
func hasLFlag(calls [][]string, socket string) bool {
	for _, c := range calls {
		for i := 0; i+1 < len(c); i++ {
			if c[i] == "-L" && c[i+1] == socket {
				return true
			}
		}
	}
	return false
}

// TestResolve_SwarmCarriesSocket: a swarm teammate's pane lives on the private
// socket cc-fleet-swarm-<team>; Resolve must surface that socket (+ the config
// pane id) for ALL target syntaxes so the CLI routes hide/show to the right
// server (otherwise the CLI would call HideRef(...,"","") and hit the default
// server).
func TestResolve_SwarmCarriesSocket(t *testing.T) {
	setup(t)
	seedSwarmTeam(t, "sw", "alice", "%42")
	wantSocket := "cc-fleet-swarm-sw"

	cases := []struct {
		name, target string
	}{
		{"pane", "%42"},
		{"team/member", "sw/alice"},
		{"name@team", "alice@sw"},
		{"bare-team", "sw"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.target)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.target, err)
			}
			if len(got) != 1 {
				t.Fatalf("Resolve(%q) returned %d targets, want 1", c.target, len(got))
			}
			if got[0].Socket != wantSocket {
				t.Fatalf("Resolve(%q).Socket = %q, want %q", c.target, got[0].Socket, wantSocket)
			}
			if got[0].PaneID != "%42" {
				t.Fatalf("Resolve(%q).PaneID = %q, want %%42", c.target, got[0].PaneID)
			}
		})
	}
}

// TestHideShowRef_SwarmRefused locks the invariant that hide/show is an
// IN-TMUX-ONLY feature. A swarm teammate's pane lives on the private socket
// cc-fleet-swarm-<team> (a detached server the operator isn't attached to) and
// the swarm session has no leader-pane anchor — break-pane'ing the last teammate
// destroys claude-swarm and orphans every hidden pane's origin window, so show
// could never rejoin. HideRef/ShowRef must therefore REFUSE with
// SWARM_UNSUPPORTED and touch tmux not at all (no -L routing, no break/join-pane).
// Resolve still carries the socket (TestResolve_SwarmCarriesSocket) — that's
// exactly the signal the gate keys on.
func TestHideShowRef_SwarmRefused(t *testing.T) {
	wantSocket := "cc-fleet-swarm-sw"

	t.Run("hide", func(t *testing.T) {
		argsPath := setup(t)
		t.Setenv("MOCK_DISPLAY_OUT", "claude-swarm:0\n")
		seedSwarmTeam(t, "sw", "alice", "%42")

		res := HideRef("sw", "alice", wantSocket, "%42")
		if res.OK || res.ErrorCode != ErrSwarmUnsupported {
			t.Fatalf("HideRef swarm: %+v, want refusal with %s", res, ErrSwarmUnsupported)
		}
		calls := recordedCalls(t, argsPath)
		if hasLFlag(calls, wantSocket) || hasSub(calls, "break-pane") {
			t.Fatalf("refused hide still hit tmux; calls=%v", calls)
		}
	})

	t.Run("show", func(t *testing.T) {
		argsPath := setup(t)
		t.Setenv("MOCK_DISPLAY_OUT", "claude-swarm:0\n")
		// Member starts hidden with a recorded origin — even so, show must refuse.
		if err := spawn.EnsureTeamDir("sw"); err != nil {
			t.Fatalf("EnsureTeamDir: %v", err)
		}
		m := mkMember("alice", "%42", true, "claude-swarm:0")
		m.Socket = wantSocket
		tc := &spawn.TeamConfig{LeadSessionID: "lead", Members: []spawn.Member{m}, Raw: map[string]any{}}
		tc.SetTmuxSocket(wantSocket)
		if err := spawn.WriteTeamConfig("sw", tc); err != nil {
			t.Fatalf("WriteTeamConfig: %v", err)
		}

		res := ShowRef("sw", "alice", wantSocket, "%42")
		if res.OK || res.ErrorCode != ErrSwarmUnsupported {
			t.Fatalf("ShowRef swarm: %+v, want refusal with %s", res, ErrSwarmUnsupported)
		}
		calls := recordedCalls(t, argsPath)
		if hasLFlag(calls, wantSocket) || hasSub(calls, "join-pane") {
			t.Fatalf("refused show still hit tmux; calls=%v", calls)
		}
	})
}

// TestHideRef_StaleConfig_RefusesMismatchedPane: when the board passes a LIVE
// paneID along with the name, HideRef must REFUSE if the config-recorded pane id
// for that name no longer matches. A by-name-only lookup would silently target
// the config-recorded pane — which could be a different teammate altogether
// after a teardown-and-respawn.
//
// Setup: config has member "alice" at pane %old, but the board's discovery
// captured "alice" at pane %new (e.g. the user respawned the teammate, but
// hadn't reloaded the board yet, and there's some stale data path). HideRef
// is called with paneID=%new — it must refuse, NOT silently hide %old.
func TestHideRef_StaleConfig_RefusesMismatchedPane(t *testing.T) {
	setup(t)
	seedTeam(t, "team", []spawn.Member{mkMember("alice", "%old", false, "")})

	res := HideRef("team", "alice", "", "%new")
	if res.OK {
		t.Fatalf("HideRef succeeded against stale config: %+v", res)
	}
	if res.ErrorCode != ErrMemberNotFound {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrMemberNotFound)
	}
	// Reload the config and confirm no hide-side mutation happened.
	tc, err := spawn.LoadTeamConfig("team")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if tc.Members[0].Hidden {
		t.Fatal("config Hidden=true despite stale-ref refusal")
	}
}

// TestHideRef_DuplicateNameAcrossTeams_HitsRightPane covers the duplicate-name
// case: two teams ("alpha", "beta") both have a member named "alice" but they
// own different pane ids. A board row for alpha/alice carries the alpha
// pane id; HideRef must operate on that pane, not silently re-resolve via
// just the name (the old by-name lookup was scoped to a single team, but the
// invariant should hold even within a team — see the next test).
func TestHideRef_DuplicateNameAcrossTeams_HitsRightPane(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0\n")
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%10", false, "")})
	seedTeam(t, "beta", []spawn.Member{mkMember("alice", "%20", false, "")})

	// HideRef on alpha with the alpha pane id — must succeed on alpha only.
	res := HideRef("alpha", "alice", "", "%10")
	if !res.OK {
		t.Fatalf("HideRef alpha/alice/%%10 failed: %+v", res)
	}
	// Reload both teams. alpha.alice must now be hidden; beta.alice must NOT
	// be touched.
	alpha, _ := spawn.LoadTeamConfig("alpha")
	beta, _ := spawn.LoadTeamConfig("beta")
	if !alpha.Members[0].Hidden {
		t.Fatalf("alpha/alice should be Hidden after HideRef; got %+v", alpha.Members[0])
	}
	if beta.Members[0].Hidden {
		t.Fatalf("beta/alice was incorrectly touched; got %+v", beta.Members[0])
	}
}

// TestHide_LegacyCallerStillWorks covers the backward-compatibility wrapper:
// existing callers (cmd/cc-fleet hide / show) still pass (team, name) without
// a paneID. The wrapper routes through HideRef with paneID="", which
// preserves the legacy by-name lookup so the CLI's behavior is unchanged.
func TestHide_LegacyCallerStillWorks(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0\n")
	seedTeam(t, "team", []spawn.Member{mkMember("alice", "%10", false, "")})

	res := Hide("team", "alice")
	if !res.OK {
		t.Fatalf("legacy Hide(team, alice) failed: %+v", res)
	}
	tc, _ := spawn.LoadTeamConfig("team")
	if !tc.Members[0].Hidden {
		t.Fatal("Hide wrapper did not set member.Hidden")
	}
}

// TestShowRef_StaleConfig_RefusesMismatchedPane is the show-side analog of
// the hide stale-config test — same property: a live paneID that diverges
// from config rejects rather than mis-targeting.
func TestShowRef_StaleConfig_RefusesMismatchedPane(t *testing.T) {
	setup(t)
	seedTeam(t, "team", []spawn.Member{mkMember("alice", "%old", true, "main:0")})

	res := ShowRef("team", "alice", "", "%new")
	if res.OK {
		t.Fatalf("ShowRef succeeded against stale config: %+v", res)
	}
	if res.ErrorCode != ErrMemberNotFound {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrMemberNotFound)
	}
}

// TestHideRef_PaneIDMatchesConfig_Succeeds covers the happy path: when the
// board's row paneID matches the config-recorded pane, HideRef proceeds
// exactly like Hide would have (idempotent, mutates config.Hidden=true).
func TestHideRef_PaneIDMatchesConfig_Succeeds(t *testing.T) {
	argsPath := setup(t)
	// display-message must return a non-empty session:window so capture
	// origin succeeds (the existing fakeTmux returns "" by default; install
	// a fixed one).
	t.Setenv("MOCK_DISPLAY_OUT", "main:0\n")

	seedTeam(t, "team", []spawn.Member{mkMember("alice", "%42", false, "")})

	res := HideRef("team", "alice", "", "%42")
	if !res.OK {
		t.Fatalf("HideRef matched-pane failed: code=%q msg=%q", res.ErrorCode, res.ErrorMsg)
	}
	if !res.Hidden {
		t.Fatal("HideRef matched-pane did not flip Hidden")
	}
	// At least one tmux invocation should have happened — the break-pane.
	calls := recordedCalls(t, argsPath)
	if len(calls) == 0 {
		t.Fatal("HideRef matched-pane did not invoke tmux")
	}
}

// TestResolve_MixedTeam_InTmuxMemberNotSwarm: a team can hold BOTH swarm members
// (out-of-tmux spawn → own socket + team-level socket) and in-tmux members (a
// later in-tmux spawn → Member.Socket == ""). The in-tmux member's pane is on the
// DEFAULT server, so Resolve must surface an EMPTY socket for it (so
// HideRef/ShowRef do NOT refuse it as SWARM_UNSUPPORTED) while still surfacing the
// swarm socket for the swarm member.
func TestResolve_MixedTeam_InTmuxMemberNotSwarm(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0")
	if err := spawn.EnsureTeamDir("mix"); err != nil {
		t.Fatal(err)
	}
	sw := mkMember("sw", "%10", false, "")
	sw.Socket = "cc-fleet-swarm-mix"       // swarm member: own socket recorded
	it := mkMember("it", "%20", false, "") // in-tmux member: Socket == ""
	tc := &spawn.TeamConfig{LeadSessionID: "lead", Members: []spawn.Member{sw, it}, Raw: map[string]any{}}
	tc.SetTmuxSocket("cc-fleet-swarm-mix") // team-level socket set by the earlier swarm spawn
	if err := spawn.WriteTeamConfig("mix", tc); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ target, wantSocket string }{
		{"mix/it", ""}, {"it@mix", ""}, {"%20", ""}, // in-tmux member → empty
		{"mix/sw", "cc-fleet-swarm-mix"}, {"sw@mix", "cc-fleet-swarm-mix"}, {"%10", "cc-fleet-swarm-mix"},
	} {
		got, err := Resolve(c.target)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", c.target, err)
		}
		if len(got) != 1 || got[0].Socket != c.wantSocket {
			t.Fatalf("Resolve(%q).Socket = %q, want %q", c.target, got[0].Socket, c.wantSocket)
		}
	}

	// The in-tmux member must NOT be refused (it hides normally); the swarm member must be.
	if r := HideRef("mix", "it", "", "%20"); r.ErrorCode == ErrSwarmUnsupported || !r.OK {
		t.Fatalf("in-tmux member should hide, not be refused as swarm: %+v", r)
	}
	if r := HideRef("mix", "sw", "cc-fleet-swarm-mix", "%10"); r.ErrorCode != ErrSwarmUnsupported {
		t.Fatalf("swarm member should be refused: %+v", r)
	}
}

// TestResolve_LegacyAllSwarm_FallsBackToTeamSocket preserves the legacy fallback:
// a team with NO per-member sockets but a team-level swarm socket (an old config)
// must still resolve its member to the team socket — the mixed-team fix must not
// regress this path.
func TestResolve_LegacyAllSwarm_FallsBackToTeamSocket(t *testing.T) {
	setup(t)
	if err := spawn.EnsureTeamDir("legacy"); err != nil {
		t.Fatal(err)
	}
	m := mkMember("w1", "%30", false, "") // no per-member socket
	tc := &spawn.TeamConfig{LeadSessionID: "lead", Members: []spawn.Member{m}, Raw: map[string]any{}}
	tc.SetTmuxSocket("cc-fleet-swarm-legacy")
	if err := spawn.WriteTeamConfig("legacy", tc); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve("legacy/w1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Socket != "cc-fleet-swarm-legacy" {
		t.Fatalf("legacy member socket = %q, want team fallback cc-fleet-swarm-legacy", got[0].Socket)
	}
}
