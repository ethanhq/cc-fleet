package teardown

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/ids"
)

// TargetKind is the shape of a teardown/hide/show target argument.
type TargetKind string

const (
	TargetPane  TargetKind = "pane"
	TargetAgent TargetKind = "agent"
	TargetTeam  TargetKind = "team"
)

// Target is a parsed `%N`, `name@team` or `team` argument.
type Target struct {
	Kind    TargetKind
	Raw     string
	PaneID  string
	AgentID string
	Team    string
	Socket  string // --socket, may be empty
}

var paneIDRe = regexp.MustCompile(`^%\d+$`)

// discoverFn is the discovery seam Teardown and the team form of ParseTarget
// use; tests substitute fixed rows.
var discoverFn = DiscoverTeammates

// ParseTarget parses a teardown target. A bare team must be a
// session-* team or the team of a discovered teammate. Callers report an error
// as BAD_ARGS.
func ParseTarget(arg, socket string) (Target, error) {
	t := Target{Raw: arg, Socket: socket}
	switch {
	case paneIDRe.MatchString(arg):
		t.Kind, t.PaneID = TargetPane, arg
		return t, nil
	case strings.Contains(arg, "@"):
		name, team, _ := strings.Cut(arg, "@")
		if err := ids.ValidateMemberName(name); err != nil {
			return Target{}, err
		}
		if err := ids.ValidateTeamName(team); err != nil {
			return Target{}, err
		}
		t.Kind, t.AgentID, t.Team = TargetAgent, arg, team
		return t, nil
	case sessionTeamRe.MatchString(arg):
		t.Kind, t.Team = TargetTeam, arg
		return t, nil
	}
	if ids.ValidateTeamName(arg) == nil {
		ts, err := discoverFn()
		if err != nil {
			return Target{}, fmt.Errorf("cannot check team %q: %w", arg, err)
		}
		for _, tm := range ts {
			if tm.Team == arg {
				t.Kind, t.Team = TargetTeam, arg
				return t, nil
			}
		}
	}
	return Target{}, fmt.Errorf("%q is not a pane id, an agent id or a known team", arg)
}
