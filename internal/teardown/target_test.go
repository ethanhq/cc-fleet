package teardown

import (
	"errors"
	"testing"
)

// TestParseTarget: %N, name@team and session teams parse without discovery; a
// bare non-session team must be a discovered team; anything else is an error.
func TestParseTarget(t *testing.T) {
	orig := discoverFn
	t.Cleanup(func() { discoverFn = orig })
	discoveries := 0
	discoverFn = func() ([]Teammate, error) {
		discoveries++
		return []Teammate{{AgentID: "w1@alpha", Name: "w1", Team: "alpha"}}, nil
	}

	good := []struct {
		arg, socket string
		want        Target
	}{
		{"%12", "/tmp/tmux-501/default", Target{Kind: TargetPane, Raw: "%12", PaneID: "%12", Socket: "/tmp/tmux-501/default"}},
		{"w1@session-7c8f769b", "", Target{Kind: TargetAgent, Raw: "w1@session-7c8f769b", AgentID: "w1@session-7c8f769b", Team: "session-7c8f769b"}},
		{"w@alpha@prod", "", Target{Kind: TargetAgent, Raw: "w@alpha@prod", AgentID: "w@alpha@prod", Team: "alpha@prod"}},
		{"session-7c8f769b", "", Target{Kind: TargetTeam, Raw: "session-7c8f769b", Team: "session-7c8f769b"}},
	}
	for _, tc := range good {
		got, err := ParseTarget(tc.arg, tc.socket)
		if err != nil || got != tc.want {
			t.Fatalf("ParseTarget(%q) = %+v, %v; want %+v", tc.arg, got, err, tc.want)
		}
	}
	if discoveries != 0 {
		t.Fatalf("pane, agent and session targets ran discovery %d times", discoveries)
	}

	got, err := ParseTarget("alpha", "")
	if err != nil || got.Kind != TargetTeam || got.Team != "alpha" {
		t.Fatalf("discovered team: %+v, %v", got, err)
	}

	for _, arg := range []string{"", "beta", "alpha/w1", "%x", "%", "@alpha", "w1@", "../x@alpha", "w 1@alpha", "session-XYZ"} {
		if _, err := ParseTarget(arg, ""); err == nil {
			t.Errorf("ParseTarget(%q) accepted", arg)
		}
	}

	discoverFn = func() ([]Teammate, error) { return nil, errors.New("tmux gone") }
	if _, err := ParseTarget("alpha", ""); err == nil {
		t.Fatal("discovery failure accepted a bare team")
	}
}
