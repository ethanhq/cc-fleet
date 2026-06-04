//go:build !windows

package teardown

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// readArgs parses the fake tmux argv log (installFakeTmux) into per-invocation
// argv slices.
func readArgs(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "__END__" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		if line == "" {
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

func hasArgs(calls [][]string, want []string) bool {
	for _, c := range calls {
		if reflect.DeepEqual(c, want) {
			return true
		}
	}
	return false
}

// TestAnnotateHidden_SetsFromConfig: AnnotateHidden reads each team's config and
// sets the Hidden flag accordingly; a teammate whose team config can't be read
// is left Hidden=false (best-effort).
func TestAnnotateHidden_SetsFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTeam(t, "alpha", []spawn.Member{
		{Name: "alice", AgentID: "alice@alpha", TmuxPaneID: "%10", AgentType: "general-purpose", Hidden: true, OriginWindow: "main:0"},
		{Name: "bob", AgentID: "bob@alpha", TmuxPaneID: "%11", AgentType: "general-purpose"},
	})

	out := AnnotateHidden([]Teammate{
		{Name: "alice", Team: "alpha"},
		{Name: "bob", Team: "alpha"},
		{Name: "ghostly", Team: "no-such-team"}, // unreadable config → stays false
	})
	if !out[0].Hidden {
		t.Errorf("alice should be Hidden=true")
	}
	if out[1].Hidden {
		t.Errorf("bob should be Hidden=false")
	}
	if out[2].Hidden {
		t.Errorf("teammate with an unreadable team config should stay Hidden=false")
	}
}

// TestAnnotateLeadSession_SetsFromConfig: the board can group live teammates by
// the parent Claude session already persisted in team config. JoinedAt is copied
// into the internal SpawnTime field for stable session/team ordering.
func TestAnnotateLeadSession_SetsFromConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTeam(t, "alpha", []spawn.Member{
		{Name: "alice", AgentID: "alice@alpha", TmuxPaneID: "%10", AgentType: "general-purpose", JoinedAt: 1000},
		{Name: "bob", AgentID: "bob@alpha", TmuxPaneID: "%11", AgentType: "general-purpose", JoinedAt: 2000},
	})

	out := AnnotateLeadSession([]Teammate{
		{Name: "alice", Team: "alpha"},
		{Name: "bob", Team: "alpha"},
		{Name: "ghostly", Team: "no-such-team"},
	})
	if out[0].LeadSessionID != "lead-uuid" || out[1].LeadSessionID != "lead-uuid" {
		t.Fatalf("alpha teammates should inherit lead-uuid: %+v", out)
	}
	if out[0].SpawnTime != 1000 || out[1].SpawnTime != 2000 {
		t.Fatalf("joinedAt should populate SpawnTime: %+v", out)
	}
	if out[2].LeadSessionID != "" || out[2].SpawnTime != 0 {
		t.Fatalf("unreadable team should stay unannotated: %+v", out[2])
	}
}

// TestTeardownTeam_KillsHiddenPane: break-pane doesn't change a pane id, so a
// hidden teammate is still torn down — TeardownTeam must KillPane its pane id.
func TestTeardownTeam_KillsHiddenPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t) // disarm process reaping

	seedTeam(t, "alpha", []spawn.Member{
		{Name: "alice", AgentID: "alice@alpha", TmuxPaneID: "%55", AgentType: "general-purpose", Hidden: true, OriginWindow: "main:0"},
	})

	res := TeardownTeam("alpha")
	if !res.OK {
		t.Fatalf("teardown: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if !hasArgs(readArgs(t, argsPath), []string{"kill-pane", "-t", "%55"}) {
		t.Fatalf("expected kill-pane on the hidden pane %%55; calls = %v", readArgs(t, argsPath))
	}
}
