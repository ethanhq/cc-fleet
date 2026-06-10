package spawn

import (
	"errors"
	"sync/atomic"
	"testing"
)

// rollbackRecorder captures every call to the rollback seams so a test can
// assert the cleanup path actually ran.
type rollbackRecorder struct {
	killPaneCalls   atomic.Int32
	killServerCalls atomic.Int32
	reapCalls       atomic.Int32
	lastSocket      string
	lastPane        string
	lastAgent       string
}

func (r *rollbackRecorder) install(t *testing.T) {
	t.Helper()
	origKP, origKS, origReap := killPaneOnServer, killServerOnSocket, reapAgentProcess
	t.Cleanup(func() {
		killPaneOnServer = origKP
		killServerOnSocket = origKS
		reapAgentProcess = origReap
	})
	killPaneOnServer = func(socket, paneID string) error {
		r.killPaneCalls.Add(1)
		r.lastSocket = socket
		r.lastPane = paneID
		return nil
	}
	killServerOnSocket = func(socket string) error {
		r.killServerCalls.Add(1)
		r.lastSocket = socket
		return nil
	}
	reapAgentProcess = func(agentID string) {
		r.reapCalls.Add(1)
		r.lastAgent = agentID
	}
}

// TestSpawn_RollbackOnWriteTeamConfigFailure: a successful tmux SplitWindow
// followed by a forced WriteTeamConfig failure MUST trigger pane cleanup (kill
// the pane on the right server, reap the claude process by agent id) — otherwise
// the pane + process stay live and the provider key keeps burning.
func TestSpawn_RollbackOnWriteTeamConfigFailure(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	rec := &rollbackRecorder{}
	rec.install(t)

	// Swap WriteTeamConfig for a failing stub. The pane has already been
	// created by the fake tmux at this point, so the rollback path must run.
	origWrite := writeTeamConfigFn
	t.Cleanup(func() { writeTeamConfigFn = origWrite })
	writeTeamConfigFn = func(_ string, _ *TeamConfig) error {
		return errors.New("forced failure: disk full")
	}

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "rb1", Team: "rbteam",
		Probe: false, AutoTeam: true,
	})
	if res.OK {
		t.Fatalf("Spawn should have failed; got OK=true with paneID=%q", res.PaneID)
	}
	if res.ErrorCode != ErrCodePaneCreationFailed {
		t.Fatalf("error code = %q, want %q (caller surfaces forced write fail as pane creation)", res.ErrorCode, ErrCodePaneCreationFailed)
	}

	// Load-bearing assertion: KillPane MUST have been called for the pane that
	// the fake tmux returned ("%99"). Without the rollback this is 0.
	if got := rec.killPaneCalls.Load(); got != 1 {
		t.Errorf("killPaneOnServer call count = %d, want 1 (pane orphaned!)", got)
	}
	if rec.lastPane != "%99" {
		t.Errorf("rollback killed pane %q, want %%99", rec.lastPane)
	}
	// In-tmux path: KillServer must NOT be called (we never kill the default server).
	if got := rec.killServerCalls.Load(); got != 0 {
		t.Errorf("killServerOnSocket called %d times for in-tmux spawn (should be 0)", got)
	}
	// Process reap must run with the right agent id (so any reparented claude
	// child is also killed).
	if got := rec.reapCalls.Load(); got != 1 {
		t.Errorf("reapAgentProcess call count = %d, want 1", got)
	}
	if rec.lastAgent != "rb1@rbteam" {
		t.Errorf("rollback reaped agent %q, want rb1@rbteam", rec.lastAgent)
	}
}

// TestSpawn_RollbackOnEnsureInboxFailure: same proof for the second failure
// surface — EnsureInbox after a successful WriteTeamConfig.
func TestSpawn_RollbackOnEnsureInboxFailure(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	rec := &rollbackRecorder{}
	rec.install(t)

	origInbox := ensureInboxFn
	t.Cleanup(func() { ensureInboxFn = origInbox })
	ensureInboxFn = func(_, _ string) error {
		return errors.New("forced failure: inbox quota")
	}

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "rb2", Team: "rbteam2",
		Probe: false, AutoTeam: true,
	})
	if res.OK {
		t.Fatalf("Spawn should have failed")
	}
	if got := rec.killPaneCalls.Load(); got != 1 {
		t.Errorf("killPane call count = %d, want 1", got)
	}
	if rec.lastPane != "%99" {
		t.Errorf("rollback killed pane %q, want %%99", rec.lastPane)
	}
	if got := rec.reapCalls.Load(); got != 1 {
		t.Errorf("reap call count = %d, want 1", got)
	}
}

// TestSpawn_RollbackSwarmFirstMemberKillsServer: out-of-tmux spawn path, FIRST
// member (this spawn CREATED the swarm session → createdServer=true). When a
// post-split failure happens, the rollback must kill the private server so it
// doesn't leak — there's no other member to strand. KillServer is the
// load-bearing assertion that distinguishes swarm rollback from in-tmux.
//
// MOCK_HASSESSION_EXIT=1 makes the fake report the session ABSENT, so SpawnSwarm
// takes its new-session branch and returns createdServer=true.
func TestSpawn_RollbackSwarmFirstMemberKillsServer(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX", "")                  // out-of-tmux → swarm branch
	t.Setenv("MOCK_HASSESSION_EXIT", "1") // session absent → first member creates it

	rec := &rollbackRecorder{}
	rec.install(t)

	origWrite := writeTeamConfigFn
	t.Cleanup(func() { writeTeamConfigFn = origWrite })
	writeTeamConfigFn = func(_ string, _ *TeamConfig) error {
		return errors.New("forced: swarm config write")
	}

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "swrb", Team: "swarm-rb",
		Probe: false, AutoTeam: true,
	})
	if res.OK {
		t.Fatalf("Spawn should have failed")
	}
	// KillPane on the swarm socket (NOT the default server).
	if got := rec.killPaneCalls.Load(); got != 1 {
		t.Errorf("killPane call count = %d, want 1", got)
	}
	if rec.lastSocket != "cc-fleet-swarm-swarm-rb" {
		t.Errorf("rollback killed pane on socket %q, want cc-fleet-swarm-swarm-rb", rec.lastSocket)
	}
	// Load-bearing: first member created the server → KillServer MUST run, else
	// the private tmux server leaks.
	if got := rec.killServerCalls.Load(); got != 1 {
		t.Errorf("killServer call count = %d, want 1 (swarm server leaked!)", got)
	}
}

// TestSpawn_RollbackSwarmLaterMemberKeepsServer: when the spawn that fails is a
// LATER swarm member (the session already existed → createdServer=false), the
// rollback must KillPane only its own pane and MUST NOT kill the whole server —
// doing so takes down the already-running first member.
//
// MOCK_HASSESSION_EXIT defaults to 0 (session "exists"), so SpawnSwarm takes
// its split-window/additional-teammate branch and returns createdServer=false.
func TestSpawn_RollbackSwarmLaterMemberKeepsServer(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX", "") // out-of-tmux → swarm branch
	// MOCK_HASSESSION_EXIT unset/0 → session exists → later member.

	// Seed an existing first member + the team socket so we can prove they
	// survive the failed later-member spawn.
	seedSwarmTeamWithMember(t, "swarm-rb2", "first", "%1")

	rec := &rollbackRecorder{}
	rec.install(t)

	origWrite := writeTeamConfigFn
	t.Cleanup(func() { writeTeamConfigFn = origWrite })
	writeTeamConfigFn = func(_ string, _ *TeamConfig) error {
		return errors.New("forced: swarm config write")
	}

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "second", Team: "swarm-rb2",
		Probe: false, AutoTeam: true,
	})
	if res.OK {
		t.Fatalf("Spawn should have failed")
	}
	// KillPane on the swarm socket for the FAILED pane (%99 from the fake).
	if got := rec.killPaneCalls.Load(); got != 1 {
		t.Errorf("killPane call count = %d, want 1", got)
	}
	if rec.lastPane != "%99" {
		t.Errorf("rollback killed pane %q, want %%99 (the failed later member)", rec.lastPane)
	}
	// Load-bearing assertion: KillServer MUST NOT run — the first member's
	// server has to survive.
	if got := rec.killServerCalls.Load(); got != 0 {
		t.Errorf("killServer call count = %d, want 0 (later-member rollback must not kill the running first member's server)", got)
	}
	// The seeded first member + team socket must still be on disk.
	tc, err := LoadTeamConfig("swarm-rb2")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if tc.TmuxSocket() != "cc-fleet-swarm-swarm-rb2" {
		t.Errorf("team socket = %q, want cc-fleet-swarm-swarm-rb2 (must survive later-member rollback)", tc.TmuxSocket())
	}
	foundFirst := false
	for _, m := range tc.Members {
		if m.Name == "first" {
			foundFirst = true
		}
		if m.Name == "second" {
			t.Errorf("failed later member 'second' still in config; want removed")
		}
	}
	if !foundFirst {
		t.Errorf("first member missing from config after later-member rollback")
	}
}

// TestSpawn_RollbackOnInboxFailure_UndoesMemberAndSocket: when EnsureInbox fails
// AFTER WriteTeamConfig has already persisted the new member (and, for swarm
// spawns, the tmuxSocket marker), the rollback must also unwind that durable
// state — (a) trim the member slice and (b) clear the swarm socket marker — else
// retrying the same (team, name) hits the pre-split duplicate check (step 8c)
// and can never succeed. We assert both via LoadTeamConfig after the failing
// spawn returns.
func TestSpawn_RollbackOnInboxFailure_UndoesMemberAndSocket(t *testing.T) {
	t.Run("in-tmux: member removed", func(t *testing.T) {
		f := newFixture(t)
		f.writeProvidersTOML("")
		f.writeFingerprint()

		rec := &rollbackRecorder{}
		rec.install(t)

		origInbox := ensureInboxFn
		t.Cleanup(func() { ensureInboxFn = origInbox })
		ensureInboxFn = func(_, _ string) error {
			return errors.New("forced failure: inbox quota")
		}

		res := Spawn(Request{
			Provider: "deepseek", AgentName: "undo1", Team: "undoteam",
			Probe: false, AutoTeam: true,
		})
		if res.OK {
			t.Fatalf("Spawn should have failed; got OK=true")
		}

		// Load-bearing: after rollback, the config must not contain the
		// just-added member — otherwise retry hits pre-split DUPLICATE_NAME.
		tc, err := LoadTeamConfig("undoteam")
		if err != nil {
			t.Fatalf("LoadTeamConfig: %v", err)
		}
		if len(tc.Members) != 0 {
			t.Fatalf("members after rollback = %d, want 0 (stale entry blocks retry)", len(tc.Members))
		}
	})

	t.Run("swarm first member: member removed + socket cleared", func(t *testing.T) {
		f := newFixture(t)
		f.writeProvidersTOML("")
		f.writeFingerprint()
		t.Setenv("TMUX", "")                  // out-of-tmux → swarm branch
		t.Setenv("MOCK_HASSESSION_EXIT", "1") // session absent → first member (createdServer=true)

		rec := &rollbackRecorder{}
		rec.install(t)

		origInbox := ensureInboxFn
		t.Cleanup(func() { ensureInboxFn = origInbox })
		ensureInboxFn = func(_, _ string) error {
			return errors.New("forced failure: inbox quota")
		}

		res := Spawn(Request{
			Provider: "deepseek", AgentName: "undosw", Team: "undoswarm",
			Probe: false, AutoTeam: true,
		})
		if res.OK {
			t.Fatalf("Spawn should have failed; got OK=true")
		}

		tc, err := LoadTeamConfig("undoswarm")
		if err != nil {
			t.Fatalf("LoadTeamConfig: %v", err)
		}
		if len(tc.Members) != 0 {
			t.Fatalf("members after swarm rollback = %d, want 0", len(tc.Members))
		}
		// First-member rollback (createdServer=true): the socket marker must be
		// cleared so a retry doesn't reuse a stale socket pointing at a server we
		// just killed. A LATER member's rollback must NOT clear it — see
		// TestSpawn_RollbackSwarmLaterMemberKeepsServer.
		if got := tc.TmuxSocket(); got != "" {
			t.Fatalf("tmuxSocket after swarm rollback = %q, want \"\"", got)
		}
	})
}

// seedSwarmTeamWithMember writes a team config holding one swarm member plus the
// team-level tmuxSocket marker, so a later-member rollback test can prove they
// survive. Uses production WriteTeamConfig before any seam stub is installed.
func seedSwarmTeamWithMember(t *testing.T, team, name, paneID string) {
	t.Helper()
	if err := EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	socket := SwarmSocketName(team)
	tc := &TeamConfig{
		Members: []Member{{
			AgentID:     name + "@" + team,
			Name:        name,
			AgentType:   "general-purpose",
			TmuxPaneID:  paneID,
			BackendType: "tmux",
			IsActive:    true,
			Socket:      socket,
		}},
	}
	tc.SetTmuxSocket(socket)
	if err := WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig seed: %v", err)
	}
}
