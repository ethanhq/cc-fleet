package teardown

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// Discovery seams: tests substitute these so they never need a live tmux
// server, a real process table or real lead sessions.
var (
	socketPathsFn = tmux.SocketPaths
	listPanesFn   = func(socketPath string) ([]tmux.PaneInfo, error) {
		return tmux.NewServerPath(socketPath).ListAllPanes()
	}
	captureJoinedFn = func(socketPath, paneID string, lines int) (string, error) {
		return tmux.NewServerPath(socketPath).CaptureJoined(paneID, lines)
	}
	processTableFn = procintrospect.ProcessTable
	childrenFn     = procintrospect.Children
	procStartFn    = procintrospect.ProcStart
	bySessionIDFn  = leadsession.BySessionID
	liveSessionsFn = leadsession.LiveSessions
)

// markerCaptureLines is how much pane history is searched for a launch
// failure marker.
const markerCaptureLines = 200

// legacySwarmPrefix names the private tmux servers cc-fleet 0.3.x spawned into.
const legacySwarmPrefix = "cc-fleet-swarm-"

var sessionTeamRe = regexp.MustCompile(`^session-[0-9a-f]{8}$`)

// DiscoverTeammates returns the cc-fleet teammates it can attribute on
// positive evidence alone; there is no ledger:
//  1. panes of every tmux server under the per-user socket directory;
//  2. processes whose exact argv carries --agent-id, minus ones still in the
//     shim or launcher (their argv still has --agent-type ccf-*);
//  3. each process is located in a pane by walking pane_pid subtrees;
//  4. attribution, first hit wins: --agent-type ccf-* → bypassed; a session
//     team member whose agentType is ccf-<p> and whose --settings is <p>'s
//     profile → running; a profile --settings plus 0.3.x evidence (a
//     cc-fleet-swarm-* socket or 0.3.x-only member keys) → legacy. Anything
//     else — notably a native teammate that inherited a provider lead's
//     profile --settings — is skipped: not listed, not counted, not cleaned.
//  5. dead panes whose joined capture ends in a launch failure marker for an
//     attributable member → failed;
//  6. in-process ccf-* members of any team config → bypassed;
//  7. a running teammate without a live lead is orphaned.
//
// Team configs are only read. Without tmux there is no pane information but
// the other sources are still scanned, and no error is returned: a missing
// tmux yields whatever those sources find, possibly nothing. An error is
// returned when the process table cannot be read; a dead server behind a
// stale socket is skipped.
func DiscoverTeammates() ([]Teammate, error) {
	sockets := socketPathsFn()
	if len(sockets) == 0 {
		// Still ask the server tmux itself resolves ($TMUX, else the default):
		// a -S socket outside the socket directory is found only this way.
		sockets = []string{""}
	}
	var panes []tmux.PaneInfo
	for _, sock := range sockets {
		ps, err := listPanesFn(sock)
		if errors.Is(err, exec.ErrNotFound) {
			break // no tmux: no pane information
		}
		if err != nil {
			continue
		}
		for i := range ps {
			if ps[i].SocketPath == "" {
				ps[i].SocketPath = sock
			}
		}
		panes = append(panes, ps...)
	}

	// Elsewhere procintrospect has no process table; in-process members are
	// still reported.
	procs, err := processTableFn()
	if err != nil && (runtime.GOOS == "linux" || runtime.GOOS == "darwin") {
		return nil, fmt.Errorf("process table: %w", err)
	}
	cands := map[int]procintrospect.Process{}
	self := os.Getpid()
	for _, p := range procs {
		if p.PID != self && discHasAgentID(p.Argv) && !discLaunching(p.Argv) {
			cands[p.PID] = p
		}
	}

	d := discovery{teams: map[string]*discTeam{}}
	loc := locateInPanes(panes, cands)
	pids := make([]int, 0, len(cands))
	for pid := range cands {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	var out []Teammate
	for _, pid := range pids {
		pane, inPane := loc[pid]
		if t, ok := d.classifyProcess(cands[pid], pane, inPane); ok {
			out = append(out, t)
		}
	}
	for _, p := range panes {
		if p.Dead {
			if t, ok := d.failedFromPane(p); ok {
				out = append(out, t)
			}
		}
	}
	out = append(out, inProcessBypassed()...)
	associateLeads(out)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Team != out[j].Team {
			return out[i].Team < out[j].Team
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// discArgs holds the argv flags discovery reads; the last occurrence wins
// (the launcher appends its --settings/--model after dropping CC's).
type discArgs struct {
	agentID, teamName, parentSessionID, settings, model, agentType string
}

func parseDiscArgs(argv []string) discArgs {
	var a discArgs
	for i := 0; i < len(argv); i++ {
		flag, val, inline := strings.Cut(argv[i], "=")
		if !inline {
			if i+1 >= len(argv) {
				break
			}
			val = argv[i+1]
		}
		var dst *string
		switch flag {
		case "--agent-id":
			dst = &a.agentID
		case "--team-name":
			dst = &a.teamName
		case "--parent-session-id":
			dst = &a.parentSessionID
		case "--settings":
			dst = &a.settings
		case "--model":
			dst = &a.model
		case "--agent-type":
			dst = &a.agentType
		default:
			continue
		}
		*dst = val
		if !inline {
			i++
		}
	}
	return a
}

func discHasAgentID(argv []string) bool {
	for _, tok := range argv {
		if tok == "--agent-id" || strings.HasPrefix(tok, "--agent-id=") {
			return true
		}
	}
	return false
}

// discLaunching reports a process still in the shim (/bin/sh <shim> …) or the
// launcher (cc-fleet __teammate-launch …): it has not exec'd claude yet.
func discLaunching(argv []string) bool {
	for _, tok := range argv {
		if tok == teammate.LaunchVerb {
			return true
		}
	}
	if len(argv) < 2 {
		return false
	}
	if filepath.Base(argv[1]) == teammate.ShimFileName {
		return true
	}
	shim, err := teammate.ShimPath()
	return err == nil && argv[1] == shim
}

// splitAgentID splits <name>@<team>, preferring an explicit --team-name suffix
// (0.3.x team names may contain '@').
func splitAgentID(agentID, teamName string) (name, team string) {
	if teamName != "" {
		if n, ok := strings.CutSuffix(agentID, "@"+teamName); ok && n != "" {
			return n, teamName
		}
	}
	name, team, _ = strings.Cut(agentID, "@")
	return name, team
}

// locateInPanes maps each candidate pid to the live pane whose process subtree
// contains it. The walk does not descend below a candidate.
func locateInPanes(panes []tmux.PaneInfo, cands map[int]procintrospect.Process) map[int]tmux.PaneInfo {
	loc := map[int]tmux.PaneInfo{}
	if len(cands) == 0 {
		return loc
	}
	for _, p := range panes {
		if p.Dead || p.PanePID <= 0 {
			continue
		}
		queue := []int{p.PanePID}
		seen := map[int]bool{p.PanePID: true}
		for len(queue) > 0 {
			pid := queue[0]
			queue = queue[1:]
			if _, ok := cands[pid]; ok {
				if _, dup := loc[pid]; !dup {
					loc[pid] = p
				}
				continue
			}
			for _, c := range childrenFn(pid) {
				if !seen[c] {
					seen[c] = true
					queue = append(queue, c)
				}
			}
		}
	}
	return loc
}

// classifyProcess applies DiscoverTeammates step 4 to one candidate process.
func (d *discovery) classifyProcess(p procintrospect.Process, pane tmux.PaneInfo, inPane bool) (Teammate, bool) {
	a := parseDiscArgs(p.Argv)
	name, team := splitAgentID(a.agentID, a.teamName)
	if name == "" {
		return Teammate{}, false
	}
	t := Teammate{
		AgentID: a.agentID,
		Name:    name,
		Team:    team,
		Model:   a.model,
		PID:     p.PID,
		Argv:    p.Argv,
		Backend: BackendUnknown,
	}
	if inPane {
		t.Backend = BackendTmux
		t.Socket = pane.SocketPath
		t.PaneID = pane.PaneID
		t.Hidden = pane.SessionName == tmux.HiddenSessionName
	}
	if strings.HasPrefix(a.agentType, teammate.TypePrefix) {
		at, _, _ := teammate.ParseAgentType(a.agentType)
		t.Provider = at.Provider
		t.State = StateBypassed
		return withProcStart(t), true
	}
	m, haveMember := d.member(team, a.agentID)
	settingsProvider := profileProvider(a.settings)
	if sessionTeamRe.MatchString(team) && haveMember && strings.HasPrefix(m.AgentType, teammate.TypePrefix) {
		if at, _, err := teammate.ParseAgentType(m.AgentType); err == nil && settingsProvider == at.Provider {
			t.Provider = at.Provider
			t.State = StateRunning
			t.SpawnTime = m.JoinedAt
			return withProcStart(t), true
		}
	}
	if settingsProvider != "" && (isLegacySwarm(t.Socket) || (haveMember && m.legacy)) {
		t.Provider = settingsProvider
		t.State = StateRunning
		t.Legacy = true
		if haveMember {
			t.SpawnTime = m.JoinedAt
		}
		return withProcStart(t), true
	}
	return Teammate{}, false
}

// withProcStart stamps the process start token teardown re-verifies before a kill.
func withProcStart(t Teammate) Teammate {
	t.ProcStart, _ = procStartFn(t.PID)
	return t
}

// failedFromPane applies DiscoverTeammates step 5: a dead pane whose joined capture
// ends in a failure marker. A dead pane has no argv, so attribution relies on
// the team config member (session teams) or 0.3.x evidence (legacy) only.
func (d *discovery) failedFromPane(p tmux.PaneInfo) (Teammate, bool) {
	text, err := captureJoinedFn(p.SocketPath, p.PaneID, markerCaptureLines)
	if err != nil {
		return Teammate{}, false
	}
	lines := strings.Split(text, "\n")
	var code, agentID string
	found := false
	for i := len(lines) - 1; i >= 0 && !found; i-- {
		code, agentID, found = teammate.ParseFailureLine(lines[i])
	}
	if !found {
		return Teammate{}, false
	}
	name, team := splitAgentID(agentID, "")
	if name == "" {
		return Teammate{}, false
	}
	t := Teammate{
		AgentID:   agentID,
		Name:      name,
		Team:      team,
		PaneID:    p.PaneID,
		Socket:    p.SocketPath,
		Backend:   BackendTmux,
		State:     StateFailed,
		ErrorCode: code,
		Hidden:    p.SessionName == tmux.HiddenSessionName,
	}
	m, haveMember := d.member(team, agentID)
	switch {
	case sessionTeamRe.MatchString(team) && haveMember && strings.HasPrefix(m.AgentType, teammate.TypePrefix):
		at, _, _ := teammate.ParseAgentType(m.AgentType)
		t.Provider = at.Provider
	case isLegacySwarm(p.SocketPath) || (haveMember && m.legacy):
		t.Legacy = true
	default:
		return Teammate{}, false
	}
	if haveMember {
		t.SpawnTime = m.JoinedAt
	}
	return t, true
}

// inProcessBypassed applies DiscoverTeammates step 6: active ccf-* members that CC
// runs in-process, found only in team configs (no pid, no pane).
func inProcessBypassed() []Teammate {
	root := claudepaths.Teams()
	if root == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*", "config.json"))
	var out []Teammate
	for _, path := range paths {
		tc, ok := readDiscTeam(path)
		if !ok {
			continue
		}
		team := tc.Name
		if team == "" {
			team = filepath.Base(filepath.Dir(path))
		}
		for _, m := range tc.members {
			if !strings.HasPrefix(m.AgentType, teammate.TypePrefix) || m.BackendType != BackendInProcess ||
				(m.IsActive != nil && !*m.IsActive) {
				continue
			}
			at, _, _ := teammate.ParseAgentType(m.AgentType)
			out = append(out, Teammate{
				AgentID:   m.AgentID,
				Name:      m.Name,
				Team:      team,
				Provider:  at.Provider,
				Backend:   BackendInProcess,
				State:     StateBypassed,
				SpawnTime: m.JoinedAt,
			})
		}
	}
	return out
}

// associateLeads applies DiscoverTeammates step 7: the lead is the session named by
// --parent-session-id unless that session leads another team, else the live
// session owning the team. A running teammate without one becomes orphaned.
func associateLeads(ts []Teammate) {
	var live []leadsession.Session
	liveLoaded := false
	for i := range ts {
		t := &ts[i]
		parent := parseDiscArgs(t.Argv).parentSessionID
		t.LeadSessionID = parent
		s, ok := leadsession.Session{}, false
		if parent != "" {
			s, ok = bySessionIDFn(parent)
			// --resume and --continue register the resumed id, so the session
			// holding the parent id now may lead another team; the team decides.
			if ok && t.Team != "" {
				if lt, found := teammate.FindLeadTeam(s); found && lt.Name != t.Team {
					ok = false
				}
			}
		}
		if !ok && t.Team != "" {
			if !liveLoaded {
				live, liveLoaded = liveSessionsFn(), true
			}
			s, ok = teammate.LeadForTeam(t.Team, live)
		}
		if ok {
			t.LeadPID = s.PID
			t.LeadSessionID = s.SessionID
		} else if t.State == StateRunning {
			t.State = StateOrphaned
		}
	}
}

// profileProvider returns <p> when settings is <profile.ProfilesDir()>/<p>.json
// for a valid provider name, else "".
func profileProvider(settings string) string {
	if settings == "" {
		return ""
	}
	dir, err := profile.ProfilesDir()
	if err != nil || filepath.Clean(filepath.Dir(settings)) != filepath.Clean(dir) {
		return ""
	}
	name, ok := strings.CutSuffix(filepath.Base(settings), ".json")
	if !ok || ids.ValidateProviderName(name) != nil {
		return ""
	}
	return name
}

func isLegacySwarm(socketPath string) bool {
	return socketPath != "" && strings.HasPrefix(filepath.Base(socketPath), legacySwarmPrefix)
}

// discovery caches the team configs one DiscoverTeammates call reads.
type discovery struct {
	teams map[string]*discTeam // by config path; nil entry: unreadable or missing
}

// discTeam is the part of a team config.json discovery reads. Claude Code
// owns the file; cc-fleet never writes it.
type discTeam struct {
	Name    string
	members []discMember
}

type discMember struct {
	AgentID     string `json:"agentId"`
	Name        string `json:"name"`
	AgentType   string `json:"agentType"`
	BackendType string `json:"backendType"`
	IsActive    *bool  `json:"isActive"`
	JoinedAt    int64  `json:"joinedAt"`
	legacy      bool   // carries a 0.3.x-only member key (leadSessionId / tmuxSocket)
}

// member returns team's config member whose agentId is agentID. Claude Code's
// sanitized directory is read first; 0.3.x kept the config under the raw team
// name, so a valid raw name that sanitizes differently is read next.
func (d *discovery) member(team, agentID string) (discMember, bool) {
	root := claudepaths.Teams()
	if team == "" || agentID == "" || root == "" {
		return discMember{}, false
	}
	dirs := []string{teammate.SanitizeTeamDir(team)}
	if team != dirs[0] && ids.ValidateTeamName(team) == nil {
		dirs = append(dirs, team)
	}
	for _, dir := range dirs {
		path := filepath.Join(root, dir, "config.json")
		if dir == "" || ids.EnsureUnderRoot(root, path) != nil {
			continue
		}
		tc, seen := d.teams[path]
		if !seen {
			tc = nil
			if loaded, ok := readDiscTeam(path); ok {
				tc = &loaded
			}
			d.teams[path] = tc
		}
		if tc == nil {
			continue
		}
		for _, m := range tc.members {
			if m.AgentID == agentID {
				return m, true
			}
		}
	}
	return discMember{}, false
}

func readDiscTeam(path string) (discTeam, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return discTeam{}, false
	}
	var raw struct {
		Name    string            `json:"name"`
		Members []json.RawMessage `json:"members"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return discTeam{}, false
	}
	tc := discTeam{Name: raw.Name}
	for _, rm := range raw.Members {
		var m discMember
		var keys map[string]json.RawMessage
		if json.Unmarshal(rm, &m) != nil || json.Unmarshal(rm, &keys) != nil {
			continue
		}
		_, hasLead := keys["leadSessionId"]
		_, hasSock := keys["tmuxSocket"]
		m.legacy = hasLead || hasSock
		tc.members = append(tc.members, m)
	}
	return tc, true
}
