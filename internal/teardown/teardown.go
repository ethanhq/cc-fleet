// Package teardown discovers cc-fleet provider teammates (ps, watch) and
// removes them.
//
// There is no ledger: Teardown acts only on rows DiscoverTeammates attributes
// on positive evidence, and re-verifies each one's identity right before
// killing it. It never writes or deletes Claude Code's team
// directories, and never touches the lead or a native teammate.
package teardown

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Error codes and skip reasons of a teardown Result.
const (
	ErrCodeBadArgs              = "BAD_ARGS"
	ErrCodeAmbiguousTarget      = "AMBIGUOUS_TARGET"
	ErrCodeInternal             = "INTERNAL"
	ErrCodeUnsupportedOnWindows = "UNSUPPORTED_ON_WINDOWS"

	ReasonIdentityMismatch = "IDENTITY_MISMATCH"
	ReasonInProcess        = "IN_PROCESS"
)

// Kill seams: tests substitute them so no real process or pane is touched.
var (
	cmdlineFn  = procintrospect.Cmdline
	killPaneFn = func(socketPath, paneID string) error {
		return tmux.NewServerPath(socketPath).KillPane(paneID)
	}
)

type Killed struct {
	AgentID string `json:"agent_id"`
	PaneID  string `json:"pane_id,omitempty"`
	Socket  string `json:"tmux_socket_path,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

type Skipped struct {
	AgentID string `json:"agent_id"`
	PaneID  string `json:"pane_id,omitempty"`
	Reason  string `json:"reason"` // IDENTITY_MISMATCH | IN_PROCESS
}

type Result struct {
	OK         bool      `json:"ok"`
	Target     string    `json:"target"`
	Killed     []Killed  `json:"killed"`
	Skipped    []Skipped `json:"skipped"`
	ErrorCode  string    `json:"error_code,omitempty"`
	ErrorMsg   string    `json:"error_msg,omitempty"`
	Suggestion string    `json:"suggestion,omitempty"`
}

// Teardown kills the discovered teammates t names. Per candidate:
//   - tmux: the pid is still in the pane's subtree with the same argv and start
//     token → kill-pane, then reap (a failed dead pane must still show the
//     same failure marker);
//   - unknown backend with a pid: same argv and start token → reap, no tmux;
//   - in-process: skipped IN_PROCESS, nothing is killed.
//
// A candidate that fails re-verification is skipped IDENTITY_MISMATCH without
// any signal or kill-pane. No candidate is ok with an empty killed list.
func Teardown(t Target, dg *diag.Logger) Result {
	res := Result{OK: true, Target: t.Raw, Killed: []Killed{}, Skipped: []Skipped{}}
	if t.Kind != TargetPane && t.Kind != TargetAgent && t.Kind != TargetTeam {
		return fail(res, ErrCodeBadArgs, fmt.Sprintf("invalid target %q", t.Raw), targetSuggestion)
	}
	ts, err := discoverFn()
	if err != nil {
		return fail(res, ErrCodeInternal, fmt.Sprintf("discover teammates: %v", err), "check `cc-fleet ps --json`")
	}
	cands, sockets := selectTargets(ts, t)
	if len(sockets) > 1 {
		return fail(res, ErrCodeAmbiguousTarget,
			fmt.Sprintf("pane %s exists on several tmux servers: %s", t.PaneID, strings.Join(sockets, ", ")),
			"repeat with --socket <path>")
	}

	var failures, hints []string
	for _, c := range cands {
		if c.Backend == BackendInProcess {
			dg.Logf("teardown: %s runs in-process, skipped", c.AgentID)
			res.Skipped = append(res.Skipped, Skipped{AgentID: c.AgentID, Reason: ReasonInProcess})
			hints = appendOnce(hints, "stop in-process teammates with TaskStop in the lead")
			continue
		}
		if !Verify(c) {
			dg.Logf("teardown: %s (pane %q pid %d) failed re-verification, skipped", c.AgentID, c.PaneID, c.PID)
			res.Skipped = append(res.Skipped, Skipped{AgentID: c.AgentID, PaneID: c.PaneID, Reason: ReasonIdentityMismatch})
			hints = appendOnce(hints, "check `cc-fleet ps --json`")
			continue
		}
		k := Killed{AgentID: c.AgentID, PID: c.PID}
		var errs []string
		if c.Backend == BackendTmux {
			k.PaneID, k.Socket = c.PaneID, c.Socket
			if err := killPaneFn(c.Socket, c.PaneID); err != nil {
				errs = append(errs, err.Error())
			} else {
				dg.Logf("teardown: killed pane %s on %s", c.PaneID, c.Socket)
			}
		}
		if c.PID > 0 {
			if err := reapProcess(c.PID, c.ProcStart); err != nil {
				errs = append(errs, err.Error())
			} else {
				dg.Logf("teardown: reaped %s (pid %d)", c.AgentID, c.PID)
			}
		}
		if len(errs) > 0 {
			failures = append(failures, fmt.Sprintf("%s: %s", c.AgentID, strings.Join(errs, "; ")))
			continue
		}
		res.Killed = append(res.Killed, k)
	}
	if len(failures) > 0 {
		return fail(res, ErrCodeInternal, strings.Join(failures, "; "), "rerun the teardown, then check `cc-fleet ps --json`")
	}
	res.Suggestion = strings.Join(hints, "; ")
	return res
}

// targetSuggestion lists the accepted target forms.
const targetSuggestion = "use a pane id (%N, with --socket <path> when several tmux servers have it), an agent id (name@team) or a team name"

func fail(res Result, code, msg, suggestion string) Result {
	res.OK = false
	res.ErrorCode, res.ErrorMsg, res.Suggestion = code, msg, suggestion
	return res
}

func appendOnce(ss []string, s string) []string {
	if slices.Contains(ss, s) {
		return ss
	}
	return append(ss, s)
}

// selectTargets filters discovered rows by t. For a pane target it also
// returns the distinct sockets the matches live on; more than one means the
// pane id is ambiguous.
func selectTargets(ts []Teammate, t Target) (cands []Teammate, sockets []string) {
	for _, tm := range ts {
		switch t.Kind {
		case TargetPane:
			if tm.PaneID != t.PaneID || (t.Socket != "" && filepath.Clean(tm.Socket) != filepath.Clean(t.Socket)) {
				continue
			}
			sockets = appendOnce(sockets, tm.Socket)
		case TargetAgent:
			if tm.AgentID != t.AgentID {
				continue
			}
		case TargetTeam:
			switch tm.State {
			case StateRunning, StateOrphaned, StateFailed, StateBypassed:
			default:
				continue
			}
			if tm.Team != t.Team {
				continue
			}
		}
		cands = append(cands, tm)
	}
	return cands, sockets
}

// Verify reports whether a discovered row still names the same teammate:
//   - tmux with a pid: the pid is still in the pane's process subtree and its
//     exact argv and start token match discovery;
//   - tmux without a pid (a failed dead pane): the pane still shows a failure
//     marker for the same agent id;
//   - any other row with a pid: exact argv and start token match.
//
// Rows without a pid or pane (in-process) never verify.
func Verify(t Teammate) bool {
	switch {
	case t.Backend == BackendTmux && t.PID == 0:
		return t.State == StateFailed && markerNames(t)
	case t.Backend == BackendTmux:
		return inPaneSubtree(t) && sameProcess(t)
	case t.PID > 0:
		return sameProcess(t)
	}
	return false
}

// sameProcess re-reads pid's exact argv and start token and compares them with
// what discovery recorded; a missing record never matches.
func sameProcess(t Teammate) bool {
	if len(t.Argv) == 0 || t.ProcStart == "" {
		return false
	}
	argv, err := cmdlineFn(t.PID)
	if err != nil || !slices.Equal(argv, t.Argv) {
		return false
	}
	start, ok := procStartFn(t.PID)
	return ok && start == t.ProcStart
}

// inPaneSubtree reports whether t.PID still runs under t.PaneID on t.Socket.
func inPaneSubtree(t Teammate) bool {
	if t.Socket == "" || t.PaneID == "" {
		return false
	}
	panes, err := listPanesFn(t.Socket)
	if err != nil {
		return false
	}
	loc := locateInPanes(panes, map[int]procintrospect.Process{t.PID: {PID: t.PID}})
	p, ok := loc[t.PID]
	return ok && p.PaneID == t.PaneID
}

// markerNames reports whether the pane's last failure marker still carries
// t.AgentID.
func markerNames(t Teammate) bool {
	if t.Socket == "" || t.PaneID == "" {
		return false
	}
	text, err := captureJoinedFn(t.Socket, t.PaneID, markerCaptureLines)
	if err != nil {
		return false
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if _, agentID, ok := teammate.ParseFailureLine(lines[i]); ok {
			return agentID == t.AgentID
		}
	}
	return false
}
