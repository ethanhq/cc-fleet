//go:build !windows

package teardown

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// TestTeardownTeam_DirGoneRecoversSwarmServer: when the team dir is already gone
// but a swarm server / provider process is still alive under the team name,
// teardown must NOT bare-return — it kills the deterministic swarm server and
// reaps the ghost (both derivable from the team name), leaving no orphan dir and
// without claiming TeamRemoved (nothing the user could see was removed).
func TestTeardownTeam_DirGoneRecoversSwarmServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	h := installReapHarness(t)

	// Stand in for the /proc scan: one teammate of this team is still live.
	const ghostID, ghostPID = "w1@gonet", 4242
	orig := discoverTeamAgentIDsFn
	t.Cleanup(func() { discoverTeamAgentIDsFn = orig })
	discoverTeamAgentIDsFn = func(team string) []string {
		if team == "gonet" {
			return []string{ghostID}
		}
		return nil
	}
	h.pids[ghostID] = []int{ghostPID}

	// Deliberately do NOT seed the team — the dir is absent.
	res := TeardownTeam("gonet", nil)
	if !res.OK {
		t.Fatalf("teardown ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.TeamRemoved {
		t.Fatal("TeamRemoved should be false when the dir never existed")
	}

	// The recovery sweep recreated the dir under the lock, then removed it: no orphan.
	dir, _ := spawn.TeamDir("gonet")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("recovery left an orphan team dir: %v", err)
	}

	calls := readFakeTmuxCalls(t, argsPath)
	if want := []string{"-L", "cc-fleet-swarm-gonet", "kill-server"}; !containsCall(calls, want) {
		t.Fatalf("missing config-free kill-server on the deterministic swarm socket; calls=%v", calls)
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

// TestTeardownTeam_DirGoneIdempotentNoop: teardown of a team that never existed
// (dir absent, nothing alive) is a clean idempotent OK — it still attempts the
// config-free swarm-server kill (swallowed when no server is running) but reaps
// nothing, claims no removal, and leaves no orphan dir.
func TestTeardownTeam_DirGoneIdempotentNoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	// Simulate a dead swarm server: kill-server exits non-zero AND prints the
	// "no server running" message KillServer keys on, so the recovery's
	// best-effort kill is swallowed (no warning surfaced) — the realistic
	// "leftovers already gone" recovery case, not just "the command was attempted".
	outFile := filepath.Join(t.TempDir(), "tmux.out")
	if err := os.WriteFile(outFile, []byte("no server running on cc-fleet-swarm-nobody\n"), 0o600); err != nil {
		t.Fatalf("write mock output: %v", err)
	}
	t.Setenv("MOCK_OUTPUT_FILE", outFile)
	t.Setenv("MOCK_EXIT_CODE", "1")

	orig := discoverTeamAgentIDsFn
	t.Cleanup(func() { discoverTeamAgentIDsFn = orig })
	discoverTeamAgentIDsFn = func(string) []string { return nil }

	res := TeardownTeam("nobody", nil)
	if !res.OK {
		t.Fatalf("teardown ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.TeamRemoved {
		t.Fatal("TeamRemoved should be false for a never-existed team")
	}
	if len(res.KilledPIDs) != 0 {
		t.Fatalf("expected no kills, got %v", res.KilledPIDs)
	}
	// The dead-server kill error must be SWALLOWED — no warning surfaced.
	for _, w := range res.Warnings {
		if strings.Contains(w, "kill swarm server") {
			t.Fatalf("dead-server kill error should be swallowed, got warning: %q", w)
		}
	}
	dir, _ := spawn.TeamDir("nobody")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("idempotent no-op left an orphan team dir: %v", err)
	}
	// The recovery path still fired (deterministic socket kill attempted).
	calls := readFakeTmuxCalls(t, argsPath)
	if want := []string{"-L", "cc-fleet-swarm-nobody", "kill-server"}; !containsCall(calls, want) {
		t.Fatalf("recovery kill-server not attempted; calls=%v", calls)
	}
}
