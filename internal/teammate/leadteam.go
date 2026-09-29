package teammate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// leadTeamSkewMs is where a lead's startup window starts before startedAt when
// its process start is unknown, unusable or may have moved with the wall clock
// (see startWindow), and how much older than that window the team named after
// its id may be (see ownsByID).
const leadTeamSkewMs = 10_000

// Seams so tests can fake the lead's process start, whether it follows
// wall-clock steps, and the session registry.
var (
	leadStartMsFn   = leadsession.ProcStartMs
	leadStartDrifts = procintrospect.StartFollowsClockSteps
	registryFn      = leadsession.RegisteredSessions
)

// leadTeamConfig is the part of teams/<team>/config.json leadteam reads. Claude
// Code owns the file; cc-fleet never writes it.
type leadTeamConfig struct {
	CreatedAt     int64  `json:"createdAt"`
	LeadAgentID   string `json:"leadAgentId"`
	LeadSessionID string `json:"leadSessionId"`
	Members       []struct {
		AgentID string `json:"agentId"`
		Cwd     string `json:"cwd"`
	} `json:"members"`
}

// FindLeadTeam locates the implicit session team of lead session s. Claude Code
// creates the team first thing at startup, named after the startup session id
// and before any startup dialog, then registers sessions/<pid>.json. The
// registered id stops naming the team after /clear and after /resume inside the
// session, and from the start with --resume and --continue, which register the
// resumed id; --worktree and EnterWorktree move the registered cwd into a
// worktree while the team keeps the launch directory. So s's team is the one
// named after its id when s owns it (see ownsByID); otherwise every registered
// session first claims the team named after its own id when it owns it, then
// the rest, latest registration first, each take the latest team of their
// directory (see inProject) created in their startup window (see startWindow),
// and s's team is the one it gets. That is right when teams were created in
// the order their sessions registered, no two teams or registrations in the
// same millisecond; every registered session is a lead and every lead is
// registered; no session claims another's team by id; and each lead's team is
// of its directory and in its startup window: a team newer than a session's
// own then belongs to a session registered later, which took it first. A match
// that a tie involving s decided (teams created in the same millisecond that s
// could take, or another session registered in the same millisecond as s) is
// not reported: they cannot be told apart.
func FindLeadTeam(s leadsession.Session) (LeadTeam, bool) {
	lt, ok, sure := matchLeadTeam(s)
	return lt, ok && sure
}

// matchLeadTeam is FindLeadTeam; sure is false when a tie decided the match.
func matchLeadTeam(s leadsession.Session) (lt LeadTeam, ok, sure bool) {
	teams := claudepaths.Teams()
	if teams == "" || s.SessionID == "" {
		return LeadTeam{}, false, false
	}
	if id8 := s.SessionID[:min(8, len(s.SessionID))]; len(id8) == 8 && !strings.ContainsAny(id8, `/\.`) {
		dir := filepath.Join(teams, "session-"+id8)
		if lt, ok := readLeadTeam(dir); ok && lt.LeadSessionID == s.SessionID && ownsByID(lt, s) {
			return lt, true, true
		}
	}
	if s.Cwd == "" {
		return LeadTeam{}, false, false
	}
	paths, _ := filepath.Glob(filepath.Join(teams, "session-*", "config.json"))
	var cands []LeadTeam
	for _, p := range paths {
		if lt, ok := readLeadTeam(filepath.Dir(p)); ok {
			cands = append(cands, lt)
		}
	}
	isS := func(r leadsession.Session) bool { return r.PID == s.PID && r.SessionID == s.SessionID }
	reg := []leadsession.Session{s}
	for _, r := range registryFn() {
		if !isS(r) {
			reg = append(reg, r)
		}
	}
	// Latest registration first; the pid orders a same-millisecond tie the same
	// way for every caller.
	sort.SliceStable(reg, func(i, j int) bool {
		if reg[i].StartedAt != reg[j].StartedAt {
			return reg[i].StartedAt > reg[j].StartedAt
		}
		return reg[i].PID > reg[j].PID
	})
	idTeam := func(r leadsession.Session) int {
		return slices.IndexFunc(cands, func(lt LeadTeam) bool { return lt.LeadSessionID == r.SessionID })
	}
	var rest []leadsession.Session
	for _, r := range reg {
		if i := idTeam(r); i >= 0 && ownsByID(cands[i], r) {
			cands = slices.Delete(cands, i, i+1)
			continue
		}
		rest = append(rest, r)
	}
	lo, hi := startWindow(s)
	sure = true
	taken := make([]bool, len(cands))
	mine, seen := -1, false // seen: s has had its turn
	var unmatched []leadsession.Session
	for _, r := range rest {
		if !isS(r) && r.StartedAt == s.StartedAt && (inProject(r.Cwd, s.Cwd) || inProject(s.Cwd, r.Cwd)) {
			sure = false
		}
		rlo, rhi := startWindow(r)
		i, found, tied := latestTeam(cands, taken, r.Cwd, rlo, rhi)
		if !seen && found && tied && inProject(cands[i].LeadCwd, s.Cwd) && cands[i].CreatedAt >= lo && cands[i].CreatedAt <= hi {
			sure = false
		}
		seen = seen || isS(r)
		if !found {
			unmatched = append(unmatched, r)
			continue
		}
		taken[i] = true
		if isS(r) {
			mine = i
		}
	}
	if mine < 0 && leadStartDrifts {
		// A forward wall-clock step can move a Linux process start past the
		// lead's own team: a session that found no team in its window keeps
		// the untaken team named after its id, created before it registered.
		for _, r := range unmatched {
			i := idTeam(r)
			if i < 0 || taken[i] || cands[i].CreatedAt > r.StartedAt || !inProject(cands[i].LeadCwd, r.Cwd) {
				continue
			}
			taken[i] = true
			if isS(r) {
				mine = i
				break
			}
		}
	}
	if mine < 0 {
		return LeadTeam{}, false, false
	}
	return cands[mine], true, sure
}

// startWindow is when s's session team can have been created: from its
// process start to startedAt. Without a process start, or with one after
// startedAt (only a wall-clock step does that), it starts leadTeamSkewMs
// before startedAt; where the start follows wall-clock steps (Linux converts
// it with the current boot time), it starts no later than that either.
func startWindow(s leadsession.Session) (lo, hi int64) {
	lo = s.StartedAt - leadTeamSkewMs
	if start, ok := leadStartMsFn(s); ok && start <= s.StartedAt && (start < lo || !leadStartDrifts) {
		lo = start
	}
	return lo, s.StartedAt
}

// ownsByID reports whether lt, named after s's session id, is s's own team.
// Claude Code creates it before registering the session, so a team created
// after startedAt belongs to a later lead whose conversation s resumed inside
// the session, and one created more than leadTeamSkewMs before s's startup
// window belongs to the lead whose id s resumed at startup.
func ownsByID(lt LeadTeam, s leadsession.Session) bool {
	lo, hi := startWindow(s)
	return lt.CreatedAt <= hi && lt.CreatedAt >= lo-leadTeamSkewMs
}

// inProject reports whether a team whose lead started in teamCwd can be the
// team of a session registered in cwd: the same directory, or cwd is a Claude
// Code worktree (<root>/.claude/worktrees/<name>, where --worktree and
// EnterWorktree move the registered cwd; root is the main checkout even from a
// subdirectory or another worktree) and teamCwd is root or below it.
func inProject(teamCwd, cwd string) bool {
	if teamCwd == cwd {
		return true
	}
	sep := string(filepath.Separator)
	root, _, found := strings.Cut(cwd, sep+".claude"+sep+"worktrees"+sep)
	return found && root != "" && (teamCwd == root || strings.HasPrefix(teamCwd, root+sep))
}

// latestTeam returns the index of the latest of teams not taken, of cwd's
// directory (see inProject) and created in [lo, hi], and whether another of
// them was created in the same millisecond.
func latestTeam(teams []LeadTeam, taken []bool, cwd string, lo, hi int64) (best int, ok, tied bool) {
	best = -1
	for i, lt := range teams {
		if taken[i] || lt.CreatedAt < lo || lt.CreatedAt > hi || !inProject(lt.LeadCwd, cwd) {
			continue
		}
		switch {
		case best < 0 || lt.CreatedAt > teams[best].CreatedAt:
			best, tied = i, false
		case lt.CreatedAt == teams[best].CreatedAt:
			tied = true
		}
	}
	return best, best >= 0, tied
}

// LeadForTeam returns the live lead session that owns team, matching each
// session with FindLeadTeam's rules, but also through a tie: either lead of a
// tie is alive. A session whose id is the team's leadSessionId wins over a
// fallback match; among those, the session that registered first after the
// team was created wins.
func LeadForTeam(team string, live []leadsession.Session) (leadsession.Session, bool) {
	var fallback leadsession.Session
	found := false
	for _, s := range live {
		lt, ok, _ := matchLeadTeam(s)
		if !ok || lt.Name != team {
			continue
		}
		if lt.LeadSessionID == s.SessionID {
			return s, true
		}
		if !found || s.StartedAt < fallback.StartedAt {
			fallback, found = s, true
		}
	}
	return fallback, found
}

// readLeadTeam parses dir/config.json. It fails when the file is missing or
// unparseable, or has no leadSessionId.
func readLeadTeam(dir string) (LeadTeam, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return LeadTeam{}, false
	}
	var cfg leadTeamConfig
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.LeadSessionID == "" {
		return LeadTeam{}, false
	}
	lt := LeadTeam{
		Name:          filepath.Base(dir),
		Dir:           dir,
		LeadSessionID: cfg.LeadSessionID,
		CreatedAt:     cfg.CreatedAt,
	}
	for _, m := range cfg.Members {
		if cfg.LeadAgentID != "" && m.AgentID == cfg.LeadAgentID {
			lt.LeadCwd = m.Cwd
			break
		}
	}
	return lt, true
}
