// Package panevis orchestrates hiding and showing a teammate's tmux pane
// without killing its process. It is the lock-aware glue between the tmux
// primitives (tmux.HidePane / ShowPane) and the persisted team state
// (spawn.Member.Hidden / OriginWindow).
//
// Invariants mirrored from the spawn pipeline:
//   - flock ordering: team lock outer, server lock inner. Team config writes run
//     under WithTeamLock; the break-pane/join-pane layout mutation runs under
//     WithServerLock (it races SplitWindow at the tmux-server layer).
//   - best-effort tmux polish lives in the tmux package; here a layout op that
//     fails is reported as TMUX_FAILED and we deliberately do NOT write config.
//   - the omitempty-Raw-shadow trap: a member's prior keys are also kept in
//     Member.Raw, and rawMap layers typed fields ON TOP of Raw with omitempty
//     dropping zero values — so clearing Hidden on show requires deleting the
//     keys from Raw first, then setting the typed fields (done on both paths).
package panevis

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/spawn"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Result is the structured outcome of a single Hide/Show. JSON tags are part of
// the skill contract (mirrors spawn/subagent envelopes) — keep them stable.
type Result struct {
	OK         bool   `json:"ok"`
	Action     string `json:"action,omitempty"` // "hide" | "show"
	Team       string `json:"team,omitempty"`
	Name       string `json:"name,omitempty"`
	PaneID     string `json:"pane_id,omitempty"`
	Hidden     bool   `json:"hidden"` // the post-operation state
	ErrorCode  string `json:"error_code,omitempty"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Error codes. Skills switch on these without parsing prose.
const (
	ErrBadArgs           = "BAD_ARGS"            // empty/unparseable target
	ErrTeamNotFound      = "TEAM_NOT_FOUND"      // no config.json for the team
	ErrMemberNotFound    = "MEMBER_NOT_FOUND"    // no member with that name
	ErrPaneNotFound      = "PANE_NOT_FOUND"      // member has no pane id / pane gone
	ErrNotHidden         = "NOT_HIDDEN"          // show on a member that isn't hidden
	ErrAlreadyHidden     = "ALREADY_HIDDEN"      // reserved: hide-on-hidden is idempotent OK, never emitted as a failure
	ErrNoOrigin          = "NO_ORIGIN"           // show but OriginWindow is empty
	ErrSwarmUnsupported  = "SWARM_UNSUPPORTED"   // hide/show requested for an out-of-tmux swarm teammate (in-tmux-only feature)
	ErrTmuxFailed        = "TMUX_FAILED"         // break-pane/join-pane (or its server lock) failed
	ErrConfigWriteFailed = "CONFIG_WRITE_FAILED" // pane moved but config write failed
	ErrInternal          = "INTERNAL"            // lock/parse failure outside the domain codes
)

// Target is one resolved (team, member) pair a hide/show command acts on,
// plus the effective tmux server socket + config-recorded pane id needed to
// route the op to the RIGHT server.
//
// A swarm teammate's pane lives on the private socket cc-fleet-swarm-<team>, not
// the default server. Resolve fills Socket from the member's per-member
// Member.Socket (falling back to the team-level tmuxSocket) and PaneID from the
// config so the CLI can call HideRef/ShowRef with the socket — otherwise
// hide/show hit the default server and either fail or mutate config without
// moving the pane. PaneID also feeds HideRef's double-key check.
type Target struct {
	Team   string
	Name   string
	Socket string
	PaneID string
}

// ResolveError is the error Resolve returns for a structural failure, carrying
// the panevis error code so the command layer can report the right one
// (TEAM_NOT_FOUND / PANE_NOT_FOUND / BAD_ARGS) instead of collapsing every
// resolve failure to BAD_ARGS. Extract it with errors.As.
type ResolveError struct {
	Code string
	Msg  string
}

func (e *ResolveError) Error() string { return e.Msg }

// resolveErr builds a coded ResolveError.
func resolveErr(code, format string, args ...any) error {
	return &ResolveError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// writeTeamConfigFn is a test seam — production uses spawn.WriteTeamConfig.
// Tests fault-inject a failure after the tmux op succeeds to verify the recovery
// Suggestion path runs (root can't be blocked by chmod, so a function-level seam
// is the only way to repro the config-write-failure path hermetically).
var writeTeamConfigFn = spawn.WriteTeamConfig

// Hide moves a member's pane into the detached hidden session and records the
// state in the team config.
//
// Hide is a backward-compatible wrapper around HideRef: it resolves the
// member by name and uses default-server tmux ops. New callers (e.g. the TUI
// board that already has the live socket + paneID from discovery) should use
// HideRef directly so socket-aware tmux ops route to the right server.
func Hide(team, name string) Result { return HideRef(team, name, "", "") }

// HideRef is the canonical hide entry point. socket scopes the tmux ops to the
// right server (empty = default = in-tmux), and paneID — when non-empty — is the
// LIVE pane id from discovery, used as a sanity check against the
// config-resolved pane id.
//
// The double-key (name + paneID) defends against a stale config silently
// mis-targeting a hide action to a different pane (duplicate name across teams,
// member that was teardown-and-respawned with a fresh pane, etc.). When paneID
// is non-empty, we find the member by paneID — if the config-recorded pane
// diverges from the live pane, we refuse rather than acting on the wrong pane.
//
// paneID == "" preserves the legacy behavior (Hide wrapper, CLI cmd/cc-fleet
// hide name@team) where the live pane id isn't known to the caller.
func HideRef(team, name, socket, paneID string) Result {
	res := Result{Action: "hide", Team: team, Name: name, PaneID: paneID}
	if team == "" || name == "" {
		res.ErrorCode = ErrBadArgs
		res.ErrorMsg = "team and name are required"
		return res
	}
	// In-tmux-only: never hide a teammate that lives on a private swarm
	// server. See refuseSwarmHideShow for why.
	if refuseSwarmHideShow(&res, team, socket) {
		return res
	}
	srv := tmux.NewServer(socket)
	lockErr := config.WithTeamLock(team, func() error {
		tc, err := spawn.LoadTeamConfig(team)
		if err != nil {
			if errors.Is(err, spawn.ErrTeamNotFound) {
				res.ErrorCode = ErrTeamNotFound
				res.ErrorMsg = fmt.Sprintf("team %q not found", team)
				return nil
			}
			res.ErrorCode = ErrInternal
			res.ErrorMsg = err.Error()
			return nil
		}
		idx := resolveMember(tc, name, paneID)
		if idx < 0 {
			res.ErrorCode = ErrMemberNotFound
			res.ErrorMsg = mismatchMsg(name, paneID, team)
			return nil
		}
		// Index into the slice (not a copy) so the mutation persists into the
		// config we write.
		m := &tc.Members[idx]
		res.PaneID = m.TmuxPaneID
		if m.TmuxPaneID == "" {
			res.ErrorCode = ErrPaneNotFound
			res.ErrorMsg = fmt.Sprintf("member %q has no tmux pane", name)
			return nil
		}
		if m.Hidden {
			// Idempotent: already hidden → success, no re-break.
			res.OK = true
			res.Hidden = true
			return nil
		}
		// Capture the origin window while the pane is still visible: reflects
		// where the pane is NOW (robust to a user moving it after spawn).
		origin, derr := srv.DisplayMessage(m.TmuxPaneID, "#{session_name}:#{window_index}")
		if derr != nil || origin == "" {
			res.ErrorCode = ErrPaneNotFound
			res.ErrorMsg = fmt.Sprintf("pane %s not found (already gone?)", m.TmuxPaneID)
			return nil
		}
		// break-pane mutates server-level layout → server lock (inner). On failure
		// do NOT write config: the pane never moved.
		if serr := config.WithServerLock(func() error { return srv.HidePane(m.TmuxPaneID) }); serr != nil {
			res.ErrorCode = ErrTmuxFailed
			res.ErrorMsg = serr.Error()
			return nil
		}
		// Defeat the omitempty-Raw-shadow before setting the typed fields.
		delete(m.Raw, "hidden")
		delete(m.Raw, "originWindow")
		m.Hidden = true
		m.OriginWindow = origin
		if werr := writeTeamConfigFn(team, tc); werr != nil {
			res.ErrorCode = ErrConfigWriteFailed
			res.ErrorMsg = werr.Error()
			// tmux state and config have diverged — pane is already in
			// claude-hidden but config still says Hidden=false. Give the user
			// the exact recovery command (paneID + origin captured above) so
			// they can manually rejoin if cc-fleet is wedged.
			res.Suggestion = fmt.Sprintf(
				"pane %s is in the hidden session but state wasn't recorded. "+
					"Recover with: tmux join-pane -s %s -t %s",
				m.TmuxPaneID, m.TmuxPaneID, origin)
			return nil
		}
		res.OK = true
		res.Hidden = true
		return nil
	})
	if lockErr != nil && res.ErrorCode == "" {
		res.ErrorCode = ErrInternal
		res.ErrorMsg = lockErr.Error()
	}
	return res
}

// Show joins a hidden member's pane back into its origin window and clears the
// hidden state in the team config.
//
// Show is a backward-compatible wrapper around ShowRef (see Hide for the
// rationale).
func Show(team, name string) Result { return ShowRef(team, name, "", "") }

// ShowRef is the canonical show entry point. See HideRef for the socket /
// paneID semantics.
func ShowRef(team, name, socket, paneID string) Result {
	res := Result{Action: "show", Team: team, Name: name, PaneID: paneID}
	if team == "" || name == "" {
		res.ErrorCode = ErrBadArgs
		res.ErrorMsg = "team and name are required"
		return res
	}
	// In-tmux-only: a swarm teammate can never be in the hidden state (HideRef
	// refuses), so show is symmetrically gated for a clear message instead of a
	// cryptic join-pane failure against a destroyed claude-swarm session.
	if refuseSwarmHideShow(&res, team, socket) {
		return res
	}
	srv := tmux.NewServer(socket)
	lockErr := config.WithTeamLock(team, func() error {
		tc, err := spawn.LoadTeamConfig(team)
		if err != nil {
			if errors.Is(err, spawn.ErrTeamNotFound) {
				res.ErrorCode = ErrTeamNotFound
				res.ErrorMsg = fmt.Sprintf("team %q not found", team)
				return nil
			}
			res.ErrorCode = ErrInternal
			res.ErrorMsg = err.Error()
			return nil
		}
		idx := resolveMember(tc, name, paneID)
		if idx < 0 {
			res.ErrorCode = ErrMemberNotFound
			res.ErrorMsg = mismatchMsg(name, paneID, team)
			return nil
		}
		m := &tc.Members[idx]
		res.PaneID = m.TmuxPaneID
		if !m.Hidden {
			res.ErrorCode = ErrNotHidden
			res.ErrorMsg = fmt.Sprintf("member %q is not hidden", name)
			return nil
		}
		origin := m.OriginWindow
		if origin == "" {
			res.ErrorCode = ErrNoOrigin
			res.ErrorMsg = fmt.Sprintf("member %q has no recorded origin window", name)
			res.Suggestion = "the pane is in the hidden session; rejoin it manually with tmux join-pane"
			return nil
		}
		if m.TmuxPaneID == "" {
			res.ErrorCode = ErrPaneNotFound
			res.ErrorMsg = fmt.Sprintf("member %q has no tmux pane", name)
			return nil
		}
		// join-pane + reflow mutate server-level layout → server lock (inner).
		if serr := config.WithServerLock(func() error { return srv.ShowPane(m.TmuxPaneID, origin) }); serr != nil {
			res.ErrorCode = ErrTmuxFailed
			res.ErrorMsg = serr.Error()
			return nil
		}
		// Defeat the omitempty-Raw-shadow: without these deletes the stale Raw
		// "hidden"/"originWindow" would shadow the typed zero values and the flag
		// would never clear.
		delete(m.Raw, "hidden")
		delete(m.Raw, "originWindow")
		m.Hidden = false
		m.OriginWindow = ""
		if werr := writeTeamConfigFn(team, tc); werr != nil {
			res.ErrorCode = ErrConfigWriteFailed
			res.ErrorMsg = werr.Error()
			// tmux state and config have diverged — pane is back in the origin
			// window but config still says Hidden=true. Symmetric to Hide's
			// recovery hint, we give the user the exact tmux break-pane command
			// (matches the flag shape used by tmux.HidePane:
			// `-d -s <pane> -t claude-hidden:`). Running it returns the system
			// to hidden so the persisted Hidden=true matches reality again; the
			// user can then `cc-fleet show` once disk pressure is gone.
			res.Suggestion = fmt.Sprintf(
				"pane %s is back in window %s but state wasn't recorded. "+
					"Recover with: tmux break-pane -d -s %s -t %s:",
				m.TmuxPaneID, origin, m.TmuxPaneID, tmux.HiddenSessionName)
			return nil
		}
		res.OK = true
		res.Hidden = false
		return nil
	})
	if lockErr != nil && res.ErrorCode == "" {
		res.ErrorCode = ErrInternal
		res.ErrorMsg = lockErr.Error()
	}
	return res
}

// resolveMember finds a member by name, optionally cross-checked against a
// LIVE paneID from discovery. When paneID is non-empty, we treat the pair
// (name, paneID) as the canonical key — a name that maps to a different
// config-recorded pane (stale config, duplicate name, member rebuilt under the
// same name) is REJECTED, not silently re-resolved to the config's pane.
// paneID == "" preserves the legacy by-name lookup.
//
// Returns the index in tc.Members, or -1 when no member matches.
func resolveMember(tc *spawn.TeamConfig, name, paneID string) int {
	if paneID == "" {
		return memberIndex(tc, name)
	}
	for i := range tc.Members {
		if tc.Members[i].TmuxPaneID == paneID {
			// Sanity-check the name. A pane id is unique within a team so the
			// name match should hold; a mismatch indicates a corrupt config.
			if tc.Members[i].Name == name {
				return i
			}
			// Distinct names share a pane id → corrupt. Refuse rather than
			// acting on the wrong identity.
			return -1
		}
	}
	return -1
}

// mismatchMsg builds the ErrMemberNotFound message, distinguishing "no such
// name" from "name found but paneID differs" so the caller can see why the
// row-targeted action refused.
func mismatchMsg(name, paneID, team string) string {
	if paneID == "" {
		return fmt.Sprintf("no member %q in team %q", name, team)
	}
	return fmt.Sprintf(
		"no member %q with pane %s in team %q (config may be stale; reload the board)",
		name, paneID, team)
}

// Resolve expands a CLI target into the (team, member) pairs to act on:
//   - "%<pane>"      → the single member that owns that pane (cross-team scan)
//   - "team/member"  → that one member
//   - "name@team"    → that one member (the agent-id form)
//   - "<team>"       → every member of the team that has a tmux pane id
//
// A structural problem (empty input, unknown pane, missing team) returns an
// error; per-member existence is left to Hide/Show (which emit MEMBER_NOT_FOUND
// etc.) so a "team/member" target reports a useful code rather than a bare error.
//
// Every parsed team/member identifier is validated for path safety (no '/' /
// '\\' / '..' / absolute paths) before flowing into LoadTeamConfig or downstream
// Hide/Show. The %pane branch is exempt because pane ids are numeric and never
// reach the filesystem name layer.
func Resolve(target string) ([]Target, error) {
	if target == "" {
		return nil, resolveErr(ErrBadArgs, "empty target")
	}
	switch {
	case strings.HasPrefix(target, "%"):
		team, name, found, err := spawn.FindMemberByPane(target)
		if err != nil {
			return nil, resolveErr(ErrInternal, "scan teams for pane %s: %v", target, err)
		}
		if !found {
			return nil, resolveErr(ErrPaneNotFound, "no teammate registered for pane %s", target)
		}
		// Re-load the owning team to read this member's effective socket. The
		// %pane string IS the config pane id (FindMemberByPane matched on it).
		socket, _, _ := memberTmuxInfo(team, name)
		return []Target{{Team: team, Name: name, Socket: socket, PaneID: target}}, nil
	case strings.Contains(target, "/"):
		parts := strings.SplitN(target, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return nil, resolveErr(ErrBadArgs, "invalid team/member target %q", target)
		}
		// Use typed constructors at the boundary so the fail-fast surfaces as
		// `NewTeamID` returning an error, not a string + plain function. The
		// validators run the same rules underneath.
		if _, err := ids.NewTeamID(parts[0]); err != nil {
			return nil, resolveErr(ErrBadArgs, "%v", err)
		}
		if _, err := ids.NewAgentName(parts[1]); err != nil {
			return nil, resolveErr(ErrBadArgs, "%v", err)
		}
		socket, paneID, _ := memberTmuxInfo(parts[0], parts[1])
		return []Target{{Team: parts[0], Name: parts[1], Socket: socket, PaneID: paneID}}, nil
	case strings.Contains(target, "@"):
		at := strings.Index(target, "@")
		name, team := target[:at], target[at+1:]
		if name == "" || team == "" {
			return nil, resolveErr(ErrBadArgs, "invalid name@team target %q", target)
		}
		if _, err := ids.NewTeamID(team); err != nil {
			return nil, resolveErr(ErrBadArgs, "%v", err)
		}
		if _, err := ids.NewAgentName(name); err != nil {
			return nil, resolveErr(ErrBadArgs, "%v", err)
		}
		socket, paneID, _ := memberTmuxInfo(team, name)
		return []Target{{Team: team, Name: name, Socket: socket, PaneID: paneID}}, nil
	default:
		if _, err := ids.NewTeamID(target); err != nil {
			return nil, resolveErr(ErrBadArgs, "%v", err)
		}
		tc, err := spawn.LoadTeamConfig(target)
		if err != nil {
			if errors.Is(err, spawn.ErrTeamNotFound) {
				return nil, resolveErr(ErrTeamNotFound, "team %q not found", target)
			}
			return nil, resolveErr(ErrInternal, "load team %q: %v", target, err)
		}
		teamSocket := tc.TmuxSocket()
		perMember := teamHasPerMemberSocket(tc)
		var targets []Target
		for _, m := range tc.Members {
			if m.TmuxPaneID != "" {
				targets = append(targets, Target{
					Team:   target,
					Name:   m.Name,
					Socket: effectiveSocket(m.Socket, teamSocket, perMember),
					PaneID: m.TmuxPaneID,
				})
			}
		}
		return targets, nil
	}
}

// memberTmuxInfo loads team's config and returns the effective tmux socket +
// config-recorded pane id for the named member, routing hide/show to the right
// server for swarm panes. A load failure or missing member yields ("", "",
// false) — the caller still hands the (team, name) to HideRef/ShowRef, which
// surfaces the precise TEAM_NOT_FOUND / MEMBER_NOT_FOUND code under the team
// lock (Resolve deliberately leaves per-member existence to Hide/Show).
func memberTmuxInfo(team, name string) (socket, paneID string, ok bool) {
	tc, err := spawn.LoadTeamConfig(team)
	if err != nil {
		return "", "", false
	}
	teamSocket := tc.TmuxSocket()
	perMember := teamHasPerMemberSocket(tc)
	for _, m := range tc.Members {
		if m.Name == name {
			return effectiveSocket(m.Socket, teamSocket, perMember), m.TmuxPaneID, true
		}
	}
	return "", "", false
}

// effectiveSocket resolves which tmux server a member's pane lives on. It prefers
// the per-member socket. An empty per-member socket means the pane is on the
// DEFAULT (in-tmux) server — UNLESS the team is a legacy config that recorded no
// per-member sockets at all, in which case the team-level swarm socket is the
// only signal and we fall back to it.
//
// teamUsesPerMemberSocket distinguishes the two. When ANY member records its own
// socket, per-member identity is authoritative, so an empty member socket is a
// genuine in-tmux pane and must NOT inherit the team's swarm socket — otherwise a
// MIXED team's in-tmux member would fall back to the team swarm socket and get
// wrongly refused as SWARM_UNSUPPORTED.
func effectiveSocket(memberSocket, teamSocket string, teamUsesPerMemberSocket bool) string {
	if memberSocket != "" {
		return memberSocket
	}
	if teamUsesPerMemberSocket {
		return "" // in-tmux pane on the default server; don't inherit a swarm socket
	}
	return teamSocket // legacy: no per-member sockets recorded, the team socket is all we have
}

// teamHasPerMemberSocket reports whether any member records its own tmux socket —
// i.e. the team uses per-member socket identity, so a mixed in-tmux/swarm team is
// possible. See effectiveSocket for why this gates the team-socket fallback.
func teamHasPerMemberSocket(tc *spawn.TeamConfig) bool {
	for i := range tc.Members {
		if tc.Members[i].Socket != "" {
			return true
		}
	}
	return false
}

// refuseSwarmHideShow fills res (using res.Action) and returns true when socket
// is non-empty — i.e. the target is an out-of-tmux swarm teammate. Swarm panes
// carry the private socket cc-fleet-swarm-<team>; in-tmux panes carry "" (the
// default server) — see the (Socket, PaneID) identity invariant and spawn.go,
// where memberSocket is set ONLY in the swarm branch.
//
// Hide/show is gated to in-tmux teammates by design:
//   - Pointless: a swarm teammate runs on a DETACHED private server the operator
//     isn't attached to, so there is no visible layout to declutter.
//   - Unsafe: the swarm session holds only teammate panes (no leader-pane anchor,
//     because the lead runs OUTSIDE tmux). break-pane'ing the last teammate
//     empties the session, tmux auto-destroys it, and every hidden pane's
//     OriginWindow ("claude-swarm:N") now points at a session that no longer
//     exists — so show's join-pane fails permanently. The in-tmux path never
//     hits this because the leader pane keeps the window (and session) alive.
//
// Gating here at the panevis choke point covers BOTH the CLI (cmd/cc-fleet/hide
// → Resolve → HideRef/ShowRef with the resolved socket) and the TUI board
// (model.go hide/showTeammateCmd → HideRef/ShowRef with the discovered Socket).
func refuseSwarmHideShow(res *Result, team, socket string) bool {
	if socket == "" {
		return false
	}
	res.ErrorCode = ErrSwarmUnsupported
	res.ErrorMsg = fmt.Sprintf(
		"%s is unavailable for out-of-tmux swarm teammates: team %q runs on the detached server %s, which isn't part of your tmux view",
		res.Action, team, socket)
	res.Suggestion = fmt.Sprintf(
		"swarm teammates run detached and keep working regardless; attach with `tmux -L %s attach -t %s` to view their panes",
		socket, tmux.SwarmSessionName)
	return true
}

// memberIndex returns the index of the member named name, or -1.
func memberIndex(tc *spawn.TeamConfig, name string) int {
	for i := range tc.Members {
		if tc.Members[i].Name == name {
			return i
		}
	}
	return -1
}
