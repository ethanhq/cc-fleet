// Package panevis hides and shows a provider teammate's tmux pane without
// killing its process.
//
// Targets come only from teardown.DiscoverTeammates and are re-verified like a
// teardown kill before any tmux mutation. The pane's origin window is kept in
// the pane's own user option @ccf_origin, so nothing is written to disk and
// the record disappears with the pane. The break-pane/join-pane layout
// mutation runs under config.WithServerLock.
package panevis

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/teardown"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Result is the outcome of one hide/show. JSON tags are the skill contract.
type Result struct {
	OK         bool   `json:"ok"`
	Action     string `json:"action,omitempty"` // hide | show
	AgentID    string `json:"agent_id,omitempty"`
	Team       string `json:"team,omitempty"`
	Name       string `json:"name,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	Socket     string `json:"tmux_socket_path,omitempty"`
	Hidden     bool   `json:"hidden"` // the post-operation state
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Error codes. Skills switch on these without parsing prose.
const (
	ErrBadArgs              = "BAD_ARGS"
	ErrPaneNotFound         = "PANE_NOT_FOUND"
	ErrNotHidden            = "NOT_HIDDEN"
	ErrAlreadyHidden        = "ALREADY_HIDDEN" // reserved: hide-on-hidden is idempotent OK, never emitted as a failure
	ErrNoOrigin             = "NO_ORIGIN"
	ErrSwarmUnsupported     = "SWARM_UNSUPPORTED"
	ErrBackendUnsupported   = "BACKEND_UNSUPPORTED"
	ErrAmbiguousTarget      = "AMBIGUOUS_TARGET"
	ErrIdentityMismatch     = "IDENTITY_MISMATCH"
	ErrTmuxFailed           = "TMUX_FAILED"
	ErrUnsupportedOnWindows = "UNSUPPORTED_ON_WINDOWS"
	ErrInternal             = "INTERNAL"
)

// originOption is the pane user option holding the window id a hidden pane
// came from.
const originOption = "@ccf_origin"

const targetSuggestion = "use a pane id (%N, with --socket <path> when several tmux servers have it) or an agent id (name@team)"

// Seams: tests substitute discovery rows and identity re-verification.
var (
	discoverFn = teardown.DiscoverTeammates
	verifyFn   = teardown.Verify
)

// Hide hides the teammate target names (%N or name@team).
func Hide(target, socket string) Result { return run("hide", target, socket, HideTeammate) }

// Show restores the hidden teammate target names (%N or name@team).
func Show(target, socket string) Result { return run("show", target, socket, ShowTeammate) }

func run(action, target, socket string, fn func(teardown.Teammate) Result) Result {
	res := Result{Action: action}
	if !strings.HasPrefix(target, "%") && !strings.Contains(target, "@") {
		return fail(res, ErrBadArgs,
			fmt.Sprintf("%q is not a pane id or an agent id; hide/show no longer take a team or team/member", target),
			targetSuggestion)
	}
	t, err := teardown.ParseTarget(target, socket)
	if err != nil {
		return fail(res, ErrBadArgs, err.Error(), targetSuggestion)
	}
	ts, err := discoverFn()
	if err != nil {
		return fail(res, ErrInternal, fmt.Sprintf("discover teammates: %v", err), "check `cc-fleet ps --json`")
	}
	var cands []teardown.Teammate
	var where []string
	for _, tm := range ts {
		switch t.Kind {
		case teardown.TargetPane:
			if tm.PaneID != t.PaneID || (t.Socket != "" && filepath.Clean(tm.Socket) != filepath.Clean(t.Socket)) {
				continue
			}
		case teardown.TargetAgent:
			if tm.AgentID != t.AgentID {
				continue
			}
		}
		cands = append(cands, tm)
		where = append(where, tm.Socket+" "+tm.PaneID)
	}
	switch {
	case len(cands) == 0:
		return fail(res, ErrPaneNotFound, fmt.Sprintf("no cc-fleet teammate matches %s", target), "check `cc-fleet ps --json`")
	case len(cands) > 1:
		return fail(res, ErrAmbiguousTarget,
			fmt.Sprintf("%s matches several teammate panes: %s", target, strings.Join(where, ", ")),
			"repeat with the pane id and --socket <path>")
	}
	return fn(cands[0])
}

// HideTeammate breaks t's pane into the detached claude-hidden session,
// recording its origin window on the pane. Hiding a hidden pane is ok.
func HideTeammate(t teardown.Teammate) Result {
	res := newResult("hide", t)
	if refuse(&res, t) {
		return res
	}
	if !verifyFn(t) {
		return mismatch(res, t)
	}
	srv := tmux.NewServerPath(t.Socket)
	// t.Hidden is a discovery snapshot; a concurrent hide may have moved the
	// pane since, so its state and origin are re-read under the lock.
	lockErr := config.WithServerLock(func() error {
		win, hidden, ok := paneState(srv, t.PaneID)
		if !ok {
			res = fail(res, ErrPaneNotFound, fmt.Sprintf("pane %s not found (already gone?)", t.PaneID), "check `cc-fleet ps --json`")
			return nil
		}
		if hidden {
			res.OK, res.Hidden = true, true
			return nil
		}
		prev, err := srv.PaneOption(t.PaneID, originOption)
		if err != nil {
			res = fail(res, ErrTmuxFailed, err.Error(), "")
			return nil
		}
		if err := srv.SetPaneOption(t.PaneID, originOption, win); err != nil {
			res = fail(res, ErrTmuxFailed, err.Error(), "")
			return nil
		}
		if err := srv.HidePane(t.PaneID); err != nil {
			if prev != "" {
				_ = srv.SetPaneOption(t.PaneID, originOption, prev)
			} else {
				_ = srv.UnsetPaneOption(t.PaneID, originOption)
			}
			res = fail(res, ErrTmuxFailed, err.Error(), "")
			return nil
		}
		res.OK, res.Hidden = true, true
		return nil
	})
	if lockErr != nil && res.ErrorCode == "" {
		res = fail(res, ErrInternal, lockErr.Error(), "")
	}
	return res
}

// ShowTeammate joins t's hidden pane back into its recorded origin window and
// clears the record.
func ShowTeammate(t teardown.Teammate) Result {
	res := newResult("show", t)
	if refuse(&res, t) {
		return res
	}
	if !verifyFn(t) {
		return mismatch(res, t)
	}
	srv := tmux.NewServerPath(t.Socket)
	lockErr := config.WithServerLock(func() error {
		_, hidden, ok := paneState(srv, t.PaneID)
		if !ok {
			res = fail(res, ErrPaneNotFound, fmt.Sprintf("pane %s not found (already gone?)", t.PaneID), "check `cc-fleet ps --json`")
			return nil
		}
		res.Hidden = hidden
		if !hidden {
			res = fail(res, ErrNotHidden, fmt.Sprintf("%s is not hidden", t.AgentID), "")
			return nil
		}
		origin, err := srv.PaneOption(t.PaneID, originOption)
		if err != nil {
			res = fail(res, ErrTmuxFailed, err.Error(), "")
			return nil
		}
		if origin == "" {
			res = fail(res, ErrNoOrigin, fmt.Sprintf("pane %s has no recorded origin window", t.PaneID),
				fmt.Sprintf("rejoin it by hand: tmux -S %s join-pane -s %s -t <window>", t.Socket, t.PaneID))
			return nil
		}
		if err := srv.ShowPane(t.PaneID, origin); err != nil {
			res = fail(res, ErrTmuxFailed, err.Error(),
				fmt.Sprintf("tmux -S %s join-pane -s %s -t <window>", t.Socket, t.PaneID))
			return nil
		}
		// A stale origin is harmless: the next hide overwrites it.
		_ = srv.UnsetPaneOption(t.PaneID, originOption)
		res.OK, res.Hidden = true, false
		return nil
	})
	if lockErr != nil && res.ErrorCode == "" {
		res = fail(res, ErrInternal, lockErr.Error(), "")
	}
	return res
}

// paneState reads paneID's window id and whether it sits in the hidden
// session; ok is false when tmux cannot resolve the pane. Window ids and
// session names never contain ':'.
func paneState(srv tmux.Server, paneID string) (win string, hidden, ok bool) {
	out, err := srv.DisplayMessage(paneID, "#{window_id}:#{session_name}")
	if err != nil {
		return "", false, false
	}
	win, sess, _ := strings.Cut(out, ":")
	return win, sess == tmux.HiddenSessionName, win != ""
}

func newResult(action string, t teardown.Teammate) Result {
	return Result{
		Action: action, AgentID: t.AgentID, Team: t.Team, Name: t.Name,
		PaneID: t.PaneID, Socket: t.Socket, Hidden: t.Hidden,
	}
}

func fail(res Result, code, msg, suggestion string) Result {
	res.OK = false
	res.ErrorCode, res.ErrorMsg, res.Suggestion = code, msg, suggestion
	return res
}

func mismatch(res Result, t teardown.Teammate) Result {
	return fail(res, ErrIdentityMismatch,
		fmt.Sprintf("the process in pane %s is not the cc-fleet teammate %s; nothing was changed", t.PaneID, t.AgentID),
		"check `cc-fleet ps --json`")
}

// refuse fills res and returns true for a teammate hide/show does not manage:
// one on a detached swarm server (no visible layout to declutter, and breaking
// its last pane would destroy the session its origin points at), or one that
// is not in a tmux pane at all.
func refuse(res *Result, t teardown.Teammate) bool {
	base := filepath.Base(t.Socket)
	if t.Socket != "" && (strings.HasPrefix(base, "claude-swarm-") || strings.HasPrefix(base, "cc-fleet-swarm-")) {
		*res = fail(*res, ErrSwarmUnsupported,
			fmt.Sprintf("%s lives on a detached swarm server, which hide/show does not manage", t.AgentID),
			fmt.Sprintf("attach with `tmux -S %s attach` instead", t.Socket))
		return true
	}
	if t.Backend != teardown.BackendTmux {
		*res = fail(*res, ErrBackendUnsupported,
			fmt.Sprintf("hide/show only works for tmux panes (this teammate uses %s)", t.Backend),
			"manage it in the terminal app")
		return true
	}
	return false
}
