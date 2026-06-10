// Package teardown removes cc-fleet teammates and team state.
//
// Two entry points:
//
//   - TeardownTeam(team)  — kill every tmux pane registered to team and
//     delete ~/.claude/teams/<team>/ tree.
//   - TeardownPane(paneID) — kill a single tmux pane and detach its member
//     entry from whichever team registered it; the team directory itself is
//     preserved.
//
// Both calls follow a best-effort cleanup contract: tmux failures (pane
// already gone, server down) are recorded as warnings but do not flip the
// result to !OK. Filesystem failures (cannot remove a team dir, cannot
// rewrite config.json) DO flip OK to false so the caller can surface the
// problem. Idempotency: re-running teardown on an already-torn-down target
// returns OK with empty Panes/Members slices.
//
// Every state mutation runs inside config.WithTeamLock(team, ...) so a
// concurrent spawn / second teardown to the same team serializes correctly.
package teardown

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/spawn"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Result is the structured outcome of a teardown call. JSON tags match the
// envelope cmd/cc-fleet/teardown.go emits — keep them stable for the skill.
//
// Panes / Members list what was actually killed / removed (not what was
// targeted) so the caller can confirm "we cleaned up these N things".
// TeamRemoved is set only by TeardownTeam when the team directory was
// removed from disk.
type Result struct {
	OK          bool     `json:"ok"`
	Target      string   `json:"target,omitempty"`
	Panes       []string `json:"panes,omitempty"`
	Members     []string `json:"members,omitempty"`
	KilledPIDs  []int    `json:"killed_pids,omitempty"`
	TeamRemoved bool     `json:"team_removed,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	ErrorCode   string   `json:"error_code,omitempty"`
	ErrorMsg    string   `json:"error_msg,omitempty"`
}

// Error codes are deliberately small — teardown is a coarse operation and
// callers (skill, CLI) don't switch on these the way spawn callers do.
const (
	ErrCodeTeamRemoveFailed = "TEAM_REMOVE_FAILED"
	ErrCodeConfigWriteFail  = "CONFIG_WRITE_FAILED"
	ErrCodeInternal         = "INTERNAL"
)

// Process-reaping seams live in reap_unix.go / reap_windows.go. On unix
// reapTeammateProcess SIGTERM/grace/SIGKILLs ghost teammate processes by agent
// id (and exposes the findTeammatePIDs / signalProc / procReapGrace vars for
// tests); on Windows the teammate lane is unsupported, so it is a no-op stub.

// configFreeReap kills team's deterministic swarm server and returns the agent
// ids of any still-live teammate processes matched by --team-name — both
// derivable from the team name alone, with no config. Shared by the two
// config-less teardown paths (the dir is absent; or its config is unparseable),
// so cleanup no longer depends on an on-disk record. The caller reaps the
// returned ids after releasing the team lock.
func configFreeReap(team string) (agentIDs []string, warnings []string) {
	sock := spawn.SwarmSocketName(team)
	if err := tmux.NewServer(sock).KillServer(); err != nil {
		warnings = append(warnings, fmt.Sprintf("kill swarm server %s: %v", sock, err))
	}
	return discoverTeamAgentIDsFn(team), warnings
}

// TeardownTeam kills every tmux pane registered in team's config.json and
// removes the entire ~/.claude/teams/<team>/ directory tree.
//
// Best-effort: a missing team (never spawned, already torn down) returns
// OK with empty Panes/Members. tmux failures become warnings. Only a
// filesystem failure on RemoveAll flips OK to false.
//
// Even when the team dir is already gone, a swarm server / vendor process can
// still be alive under this team name (a prior teardown left it, or the dir was
// deleted out of band). Both the swarm socket and the per-proc --team-name are
// derivable from the team name alone, so recovery runs config-free — and under
// WithTeamLock, so it serializes with a concurrent spawn into the same team
// (the lock recreates the absent dir; RemoveAll cleans it back up).
func TeardownTeam(team string, dg *diag.Logger) Result {
	if team == "" {
		return Result{
			OK:        false,
			ErrorCode: ErrCodeInternal,
			ErrorMsg:  "team name is empty",
		}
	}

	res := Result{OK: true, Target: team}

	dir, dirErr := spawn.TeamDir(team)
	if dirErr != nil {
		res.OK = false
		res.ErrorCode = ErrCodeInternal
		res.ErrorMsg = dirErr.Error()
		return res
	}
	// Note whether a real team dir existed up front: a no-op teardown of a
	// never-created team must not claim TeamRemoved (the lock below recreates
	// then deletes the dir). We deliberately do NOT early-return on absence —
	// leftovers can outlive a deleted dir, and recovery must run under the lock.
	dirExisted := false
	if _, statErr := os.Stat(dir); statErr == nil {
		dirExisted = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		res.Warnings = append(res.Warnings, fmt.Sprintf("stat team dir: %v", statErr))
	}

	// Agent ids whose (possibly reparented) processes we reap after releasing
	// the lock — we don't want to hold the team lock during the SIGTERM grace
	// sleep.
	var reapIDs []string
	lockErr := config.WithTeamLock(team, func() error {
		dg.Logf("teardown: team lock acquired %s", team)
		// Load member list — failure here is non-fatal because we can still
		// rm -rf the team dir, which is the primary cleanup goal. We just
		// won't know which pane ids to kill or member names to report.
		tc, loadErr := spawn.LoadTeamConfig(team)
		switch {
		case loadErr == nil && tc != nil:
			// Per-member socket — out-of-tmux teams keep their panes on a private
			// swarm server (cc-fleet-swarm-<team>), and a team can mix in-tmux +
			// swarm members. KillPane MUST target the right server, or a
			// default-server kill-pane on a swarm pane "can't find pane" and is
			// swallowed as success, silently leaking the pane. Resolve order:
			// Member.Socket (per-member) → tc.TmuxSocket() (team-level legacy
			// fallback so older configs without per-member sockets still work).
			teamSock := tc.TmuxSocket()
			// Track which sockets we actually used so we can KillServer the
			// now-empty swarm servers once. dedup on socket name; "" (default)
			// is never killed.
			killedSockets := map[string]struct{}{}
			for _, m := range tc.Members {
				sock := m.Socket
				if sock == "" {
					sock = teamSock
				}
				srv := tmux.NewServer(sock)
				if m.TmuxPaneID != "" {
					if err := srv.KillPane(m.TmuxPaneID); err != nil {
						res.Warnings = append(res.Warnings,
							fmt.Sprintf("kill pane %s: %v", m.TmuxPaneID, err))
					} else {
						dg.Logf("teardown: killed pane %s (socket %q)", m.TmuxPaneID, sock)
						res.Panes = append(res.Panes, m.TmuxPaneID)
					}
				}
				if m.Name != "" {
					res.Members = append(res.Members, m.Name)
				}
				if m.AgentID != "" {
					reapIDs = append(reapIDs, m.AgentID)
				}
				if sock != "" {
					killedSockets[sock] = struct{}{}
				}
			}
			// Once every swarm pane is killed, tear down the now-empty private
			// servers so no orphaned tmux server lingers. Best-effort +
			// idempotent (KillServer swallows "no server running"). We iterate
			// over distinct sockets so a mixed-mode team with two swarm sockets
			// still gets both reaped (unusual, but consistent).
			for sock := range killedSockets {
				if err := tmux.NewServer(sock).KillServer(); err != nil {
					res.Warnings = append(res.Warnings,
						fmt.Sprintf("kill swarm server %s: %v", sock, err))
				} else {
					dg.Logf("teardown: killed swarm server %s", sock)
				}
			}
		case loadErr != nil && !errors.Is(loadErr, spawn.ErrTeamNotFound):
			// Config present but unparseable (parse error / permission): no pane
			// ids to read, but the swarm socket + ghost --team-name are derivable
			// from the team name. Recover before RemoveAll deletes the only record.
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("load team config: %v", loadErr))
			ids, warns := configFreeReap(team)
			res.Warnings = append(res.Warnings, warns...)
			reapIDs = append(reapIDs, ids...)
		default:
			// ErrTeamNotFound: the dir was absent (recreated empty by the lock) or
			// carries no config.json. A swarm server / vendor proc may still be
			// alive under this team name with no record to find it — recover
			// config-free, same as the parse-fail path but without a load warning.
			ids, warns := configFreeReap(team)
			res.Warnings = append(res.Warnings, warns...)
			reapIDs = append(reapIDs, ids...)
		}

		// Remove the entire team dir. This is the failure path that matters:
		// if we can't delete the directory, the user will see stale state on
		// next spawn / ps. TeamRemoved reflects only a real team that existed
		// before this call — a recovery sweep of an already-absent dir removes
		// nothing the user can see.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove team dir: %w", err)
		}
		dg.Logf("teardown: team dir removed (existed=%v)", dirExisted)
		if dirExisted {
			res.TeamRemoved = true
		}
		return nil
	})

	// Reap any teammate processes that survived their pane being killed.
	// Outside the lock and best-effort: warnings only, never flips OK.
	for _, id := range reapIDs {
		killed, warns := reapTeammateProcess(id)
		if len(killed) > 0 {
			dg.Logf("teardown: reaped %s (pids %v)", id, killed)
		}
		res.KilledPIDs = append(res.KilledPIDs, killed...)
		res.Warnings = append(res.Warnings, warns...)
	}

	if lockErr != nil {
		res.OK = false
		res.ErrorCode = ErrCodeTeamRemoveFailed
		res.ErrorMsg = lockErr.Error()
	}
	return res
}

// TeardownPane kills a single tmux pane (idempotent — a non-existent pane
// is OK) and detaches its member entry from whichever team registered it.
//
// If no team claims this pane (e.g. it was spawned manually, or its team
// config was already cleaned up) the pane is still killed and we return
// OK with a warning. The inbox file for the member is removed but the
// team directory itself is preserved.
//
// paneID must be a tmux pane id like "%42" — the leading "%" is the only
// shape we recognize; callers that mix in team names should use
// TeardownTeam instead.
func TeardownPane(paneID string, dg *diag.Logger) Result {
	if paneID == "" {
		return Result{
			OK:        false,
			ErrorCode: ErrCodeInternal,
			ErrorMsg:  "pane id is empty",
		}
	}
	if !strings.HasPrefix(paneID, "%") {
		return Result{
			OK:        false,
			ErrorCode: ErrCodeInternal,
			ErrorMsg:  fmt.Sprintf("pane id %q must start with %%", paneID),
		}
	}

	res := Result{OK: true, Target: paneID}

	// Reverse-lookup the team that owns this pane BEFORE killing it — once
	// killed the pane id no longer correlates to anything if the lookup
	// races with another teardown. The owning team's swarm socket (empty for an
	// in-tmux team) tells us which tmux server the pane actually lives on.
	team, memberName, swarmSocket, lookupErr := findPaneOwner(paneID)
	if lookupErr != nil {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("find pane owner: %v", lookupErr))
	}

	// Kill the pane first, on its OWNING server (default or the team's private
	// swarm socket). tmux KillPane already treats "can't find pane" as success so
	// idempotent re-runs work transparently — but only when aimed at the right
	// server (a default-server kill on a swarm pane "can't find" → leak).
	if err := tmux.NewServer(swarmSocket).KillPane(paneID); err != nil {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("kill pane %s: %v", paneID, err))
	} else {
		res.Panes = append(res.Panes, paneID)
	}

	// If no team claims this pane, we're done — return OK with whatever
	// warnings accumulated. This covers both "manually started teammate"
	// and "team already torn down" cases.
	if team == "" {
		return res
	}

	// The agent id is <name>@<team> (the spawn contract). We reap its process
	// after releasing the lock — reconstructing here rather than reading it
	// inside the lock keeps the reap working even if the team config races
	// away before we re-acquire it.
	var reapID string
	if memberName != "" {
		reapID = memberName + "@" + team
	}

	// Detach the member entry inside the team lock so we don't race a
	// concurrent spawn to the same team.
	lockErr := config.WithTeamLock(team, func() error {
		tc, err := spawn.LoadTeamConfig(team)
		if err != nil {
			if errors.Is(err, spawn.ErrTeamNotFound) {
				// Team config disappeared between findPaneOwner and the
				// lock — treat as already-cleaned.
				return nil
			}
			return fmt.Errorf("load team config: %w", err)
		}

		// Filter members in place; mark which one we removed for the result.
		kept := tc.Members[:0]
		for _, m := range tc.Members {
			if m.TmuxPaneID == paneID {
				res.Members = append(res.Members, m.Name)
				continue
			}
			kept = append(kept, m)
		}
		tc.Members = kept

		// If that was the last pane on an out-of-tmux swarm server, tear the
		// now-empty private server down too and drop the socket marker so it
		// doesn't leak. Best-effort; team-level teardown also handles this.
		if sock := tc.TmuxSocket(); sock != "" && len(tc.Members) == 0 {
			if err := tmux.NewServer(sock).KillServer(); err != nil {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("kill swarm server %s: %v", sock, err))
			} else {
				tc.SetTmuxSocket("")
			}
		}

		if err := spawn.WriteTeamConfig(team, tc); err != nil {
			return fmt.Errorf("write team config: %w", err)
		}

		// Remove the inbox file. memberName is non-empty here because
		// findPaneOwner returned a match. Missing inbox is not an error
		// (idempotent), only stat failure with a real errno would be.
		inboxPath, ipErr := spawn.InboxPath(team, memberName)
		if ipErr == nil {
			if err := os.Remove(inboxPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				// Don't fail the whole teardown for an inbox we can't
				// delete — the member entry is already gone, so a future
				// spawn will overwrite it.
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("remove inbox %s: %v", inboxPath, err))
			}
		}
		return nil
	})

	// Reap the (possibly reparented) teammate process outside the lock.
	// Best-effort: warnings only, never flips OK.
	if reapID != "" {
		killed, warns := reapTeammateProcess(reapID)
		res.KilledPIDs = append(res.KilledPIDs, killed...)
		res.Warnings = append(res.Warnings, warns...)
	}

	if lockErr != nil {
		res.OK = false
		res.ErrorCode = ErrCodeConfigWriteFail
		res.ErrorMsg = lockErr.Error()
	}
	return res
}

// findPaneOwner scans every team's config.json looking for a member whose
// tmuxPaneId == paneID. Returns (team, memberName, socket, nil) on match, where
// socket is the OWNING member's socket — Member.Socket when set, falling back
// to TeamConfig.TmuxSocket() for older configs that pre-date per-member sockets.
//
// No match returns ("", "", "", nil) — caller treats that as "manually started
// teammate, just kill the pane on the default server". A real I/O error returns
// (..., err).
func findPaneOwner(paneID string) (team, name, socket string, err error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", "", "", errors.New("HOME is not set")
	}
	teamsRoot := filepath.Join(home, ".claude", "teams")

	entries, err := os.ReadDir(teamsRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No teams root means no teams — not an error, just no match.
			return "", "", "", nil
		}
		return "", "", "", fmt.Errorf("read teams root: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		teamName := e.Name()
		tc, loadErr := spawn.LoadTeamConfig(teamName)
		if loadErr != nil {
			// Skip teams whose config is unreadable / missing — they cannot
			// own this pane.
			continue
		}
		for _, m := range tc.Members {
			if m.TmuxPaneID == paneID {
				memSock := m.Socket
				if memSock == "" {
					memSock = tc.TmuxSocket()
				}
				return teamName, m.Name, memSock, nil
			}
		}
	}
	return "", "", "", nil
}
