//go:build !windows

package teardown

import (
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// TestTeardownTeam_ParseFailureStillKillsSwarmServer: when a team's config can't
// be parsed, teardown can't read pane ids — but it must still kill the swarm
// server (derivable from the team name) before RemoveAll deletes the only
// record, otherwise the panes/processes leak with nothing left to target them.
func TestTeardownTeam_ParseFailureStillKillsSwarmServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	// Seed a real team dir, then corrupt config.json so LoadTeamConfig fails.
	dir := seedTeam(t, "brokt", []spawn.Member{
		{Name: "w1", AgentID: "w1@brokt", TmuxPaneID: "%0", AgentType: "general-purpose"},
	})
	cfgPath, _ := spawn.TeamConfigPath("brokt")
	if err := os.WriteFile(cfgPath, []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	res := TeardownTeam("brokt", nil)
	if !res.OK {
		t.Fatalf("teardown ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if !res.TeamRemoved {
		t.Fatal("TeamRemoved should be true even on a corrupt config")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("team dir still exists: %v", err)
	}

	calls := readFakeTmuxCalls(t, argsPath)
	want := []string{"-L", "cc-fleet-swarm-brokt", "kill-server"}
	if !containsCall(calls, want) {
		t.Fatalf("missing config-free kill-server on the deterministic swarm socket; calls=%v", calls)
	}

	var sawLoadWarning bool
	for _, w := range res.Warnings {
		if strings.Contains(w, "load team config") {
			sawLoadWarning = true
		}
	}
	if !sawLoadWarning {
		t.Fatalf("expected a load-team-config warning; warnings=%v", res.Warnings)
	}
}

// TestTeardownTeam_ParseFailureReapsGhostProcess: on a corrupt config, a ghost
// process discovered by team name (config-free) must be reaped before the dir
// is removed, so it can't be orphaned with no record left to find it.
func TestTeardownTeam_ParseFailureReapsGhostProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	h := installReapHarness(t)

	// The discovery seam stands in for the /proc scan: pretend one teammate of
	// this team is still live, with a known pid for the reap harness to "kill".
	const ghostID, ghostPID = "w1@brokt", 4242
	orig := discoverTeamAgentIDsFn
	t.Cleanup(func() { discoverTeamAgentIDsFn = orig })
	discoverTeamAgentIDsFn = func(team string) []string {
		if team == "brokt" {
			return []string{ghostID}
		}
		return nil
	}
	h.pids[ghostID] = []int{ghostPID}

	seedTeam(t, "brokt", []spawn.Member{{Name: "w1", AgentID: ghostID, AgentType: "general-purpose"}})
	cfgPath, _ := spawn.TeamConfigPath("brokt")
	if err := os.WriteFile(cfgPath, []byte("{ not valid json"), 0o600); err != nil {
		t.Fatalf("corrupt config: %v", err)
	}

	res := TeardownTeam("brokt", nil)
	if !res.OK {
		t.Fatalf("teardown ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	var reaped bool
	for _, p := range res.KilledPIDs {
		if p == ghostPID {
			reaped = true
		}
	}
	if !reaped {
		t.Fatalf("ghost pid %d not reaped; KilledPIDs=%v", ghostPID, res.KilledPIDs)
	}
	if sigs := h.sigs(ghostPID); len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
		t.Fatalf("ghost pid %d: signals=%v, want SIGTERM first", ghostPID, sigs)
	}
}
