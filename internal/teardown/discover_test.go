//go:build !windows

package teardown

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

const (
	discTeamName = "session-7c8f769b"
	discLeadID   = "7c8f769b-0000-4000-8000-000000000001"
	discSock     = "/tmp/tmux-501/default"
	discClaude   = "/home/u/.local/share/claude/versions/2.1.281"
	discJoinedAt = int64(1_700_000_000_000)
)

// discFixture fakes every discovery seam and points HOME, CLAUDE_CONFIG_DIR and
// XDG_CONFIG_HOME at temp dirs, so no test touches tmux, the real process table
// or the real ~/.claude.
type discFixture struct {
	t        *testing.T
	profiles string
	sockets  []string
	panes    map[string][]tmux.PaneInfo // socket path → panes; absent = dead server
	children map[int][]int
	procs    []procintrospect.Process
	captures map[string]string // "<socket> <pane>" → joined capture text
	capLines []int
	sessions map[string]leadsession.Session // live sessions by id
	live     []leadsession.Session

	origListPanes func(string) ([]tmux.PaneInfo, error)
	origCapture   func(string, string, int) (string, error)
	origTable     func() ([]procintrospect.Process, error)
	origChildren  func(int) []int
}

func discSetup(t *testing.T) *discFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude-config"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))

	f := &discFixture{
		t:        t,
		profiles: filepath.Join(home, ".claude", "profiles"),
		panes:    map[string][]tmux.PaneInfo{},
		children: map[int][]int{},
		captures: map[string]string{},
		sessions: map[string]leadsession.Session{},

		origListPanes: listPanesFn,
		origCapture:   captureJoinedFn,
		origTable:     processTableFn,
		origChildren:  childrenFn,
	}
	origSockets, origStart, origByID, origLive := socketPathsFn, procStartFn, bySessionIDFn, liveSessionsFn
	t.Cleanup(func() {
		socketPathsFn, listPanesFn, captureJoinedFn = origSockets, f.origListPanes, f.origCapture
		processTableFn, childrenFn, procStartFn = f.origTable, f.origChildren, origStart
		bySessionIDFn, liveSessionsFn = origByID, origLive
	})

	socketPathsFn = func() []string { return f.sockets }
	listPanesFn = func(sock string) ([]tmux.PaneInfo, error) {
		ps, ok := f.panes[sock]
		if !ok {
			return nil, errors.New("tmux list-panes: no server running")
		}
		return ps, nil
	}
	captureJoinedFn = func(sock, pane string, lines int) (string, error) {
		f.capLines = append(f.capLines, lines)
		text, ok := f.captures[sock+" "+pane]
		if !ok {
			return "", errors.New("can't find pane")
		}
		return text, nil
	}
	processTableFn = func() ([]procintrospect.Process, error) { return f.procs, nil }
	childrenFn = func(pid int) []int { return f.children[pid] }
	procStartFn = func(pid int) (string, bool) { return "start-" + strconv.Itoa(pid), true }
	bySessionIDFn = func(id string) (leadsession.Session, bool) {
		s, ok := f.sessions[id]
		return s, ok
	}
	liveSessionsFn = func() []leadsession.Session { return f.live }
	return f
}

// pane registers a live pane on sock whose shell (panePID) is the parent of pid.
func (f *discFixture) pane(sock, paneID, session string, panePID, pid int) {
	if !discContains(f.sockets, sock) {
		f.sockets = append(f.sockets, sock)
	}
	f.panes[sock] = append(f.panes[sock], tmux.PaneInfo{
		SocketPath: sock, SessionName: session, WindowID: "@1", PaneID: paneID, PanePID: panePID,
	})
	if pid != 0 {
		f.children[panePID] = append(f.children[panePID], pid)
	}
}

// deadPane registers a dead pane on sock whose joined capture is text.
func (f *discFixture) deadPane(sock, paneID, text string) {
	if !discContains(f.sockets, sock) {
		f.sockets = append(f.sockets, sock)
	}
	f.panes[sock] = append(f.panes[sock], tmux.PaneInfo{
		SocketPath: sock, SessionName: "main", WindowID: "@1", PaneID: paneID, PanePID: 0, Dead: true, DeadStatus: 1,
	})
	f.captures[sock+" "+paneID] = text
}

func (f *discFixture) proc(pid int, argv ...string) {
	f.procs = append(f.procs, procintrospect.Process{PID: pid, Argv: argv})
}

func (f *discFixture) profile(provider string) string {
	return filepath.Join(f.profiles, provider+".json")
}

// team writes teams/<dir>/config.json with a lead row plus members.
func (f *discFixture) team(dir string, members ...map[string]any) {
	f.t.Helper()
	f.teamOf(dir, discLeadID, discJoinedAt, members...)
}

// teamOf is team with the lead session id and createdAt given.
func (f *discFixture) teamOf(dir, leadID string, createdAt int64, members ...map[string]any) {
	f.t.Helper()
	all := append([]map[string]any{{
		"agentId": "team-lead@" + dir, "name": "team-lead", "agentType": "team-lead",
		"tmuxPaneId": "leader", "backendType": "in-process", "cwd": "/work",
	}}, members...)
	cfg := map[string]any{
		"name": dir, "createdAt": createdAt, "leadAgentId": "team-lead@" + dir,
		"leadSessionId": leadID, "members": all,
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "teams", dir, "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func discMemberRow(agentID, agentType string) map[string]any {
	name, _, _ := strings.Cut(agentID, "@")
	return map[string]any{
		"agentId": agentID, "name": name, "agentType": agentType, "joinedAt": discJoinedAt,
		"tmuxPaneId": "%3", "backendType": "tmux", "cwd": "/work", "subscriptions": []string{},
	}
}

// discArgv is the argv Claude Code gives a pane teammate, with --settings and
// --model last as the launcher leaves them.
func discArgv(agentID, settings, model string, extra ...string) []string {
	name, team, _ := strings.Cut(agentID, "@")
	argv := []string{discClaude, "--agent-id", agentID, "--agent-name", name, "--team-name", team,
		"--agent-color", "blue", "--parent-session-id", discLeadID}
	argv = append(argv, extra...)
	if settings != "" {
		argv = append(argv, "--settings", settings)
	}
	return append(argv, "--model", model)
}

func discRun(t *testing.T) []Teammate {
	t.Helper()
	got, err := DiscoverTeammates()
	if err != nil {
		t.Fatalf("DiscoverTeammates: %v", err)
	}
	return got
}

func discByID(ts []Teammate, agentID string) (Teammate, bool) {
	for _, t := range ts {
		if t.AgentID == agentID {
			return t, true
		}
	}
	return Teammate{}, false
}

func discContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestDiscoverProviderTeammate: a session team member whose agentType is
// ccf-<p> and whose --settings is <p>'s profile is a running teammate with
// every ps field filled from discovery.
func TestDiscoverProviderTeammate(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName,
		discMemberRow("worker@"+discTeamName, "ccf-glm"),
		discMemberRow("deep@"+discTeamName, "ccf-glm.strong"),
	)
	f.sessions[discLeadID] = leadsession.Session{PID: 100, SessionID: discLeadID}
	argv := discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6", "--permission-mode", "default")
	f.proc(501, argv...)
	f.pane(discSock, "%3", "main", 500, 501)
	f.proc(601, discArgv("deep@"+discTeamName, f.profile("glm"), "glm-5")...)
	f.pane(discSock, "%4", tmux.HiddenSessionName, 600, 601)

	got := discRun(t)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	want := Teammate{
		AgentID: "worker@" + discTeamName, Name: "worker", Team: discTeamName, PaneID: "%3",
		Provider: "glm", Model: "glm-4.6", PID: 501, ProcStart: "start-501", Argv: argv,
		Socket: discSock, Backend: BackendTmux, LeadPID: 100, LeadSessionID: discLeadID,
		State: StateRunning, SpawnTime: discJoinedAt,
	}
	if w, _ := discByID(got, want.AgentID); !reflect.DeepEqual(w, want) {
		t.Fatalf("worker row:\n got %+v\nwant %+v", w, want)
	}
	deep, _ := discByID(got, "deep@"+discTeamName)
	if deep.State != StateRunning || deep.Provider != "glm" || !deep.Hidden || deep.Legacy {
		t.Fatalf("slot teammate row = %+v, want running glm hidden non-legacy", deep)
	}
}

// TestDiscoverCurrentServerOutsideSocketDir: with no socket under the tmux
// socket directory, the server tmux itself resolves ($TMUX — a -S socket
// elsewhere) is still listed, and its rows carry that socket.
func TestDiscoverCurrentServerOutsideSocketDir(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
	f.sessions[discLeadID] = leadsession.Session{PID: 100, SessionID: discLeadID}
	f.proc(501, discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6")...)
	const custom = "/work/sockets/session.sock"
	f.panes[""] = []tmux.PaneInfo{{SocketPath: custom, SessionName: "main", WindowID: "@1", PaneID: "%3", PanePID: 500}}
	f.children[500] = []int{501}

	w, ok := discByID(discRun(t), "worker@"+discTeamName)
	if !ok || w.State != StateRunning || w.PaneID != "%3" || w.Socket != custom {
		t.Fatalf("worker row = %+v (found %v), want running on %%3 at %s", w, ok, custom)
	}
}

// TestDiscoverSkipsLaunching: a process still in the shim or the launcher
// carries --agent-type ccf-* but is not a bypassed teammate.
func TestDiscoverSkipsLaunching(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
	shim, err := teammate.ShimPath()
	if err != nil {
		t.Fatal(err)
	}
	id := "worker@" + discTeamName
	f.proc(501, "/bin/sh", shim, "--agent-id", id, "--agent-type", "ccf-glm")
	f.proc(502, "/bin/sh", "/elsewhere/bin/"+teammate.ShimFileName, "--agent-id", id, "--agent-type", "ccf-glm")
	f.proc(503, "/usr/local/bin/cc-fleet", teammate.LaunchVerb, "--agent-id", id, "--agent-type", "ccf-glm")
	f.pane(discSock, "%3", "main", 500, 501)

	if got := discRun(t); len(got) != 0 {
		t.Fatalf("launching processes were listed: %+v", got)
	}
}

// TestDiscoverBypassedArgv: --agent-type ccf-* still in a claude argv means the
// launcher never ran. It is checked first, so a profile --settings next to it
// does not make the row running.
func TestDiscoverBypassedArgv(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
	f.proc(501, discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6", "--agent-type", "ccf-glm")...)
	f.pane(discSock, "%3", "main", 500, 501)
	// No profile and no pane: still bypassed, with an unknown backend.
	f.proc(701, discArgv("fast@"+discTeamName, "", "sonnet", "--agent-type=ccf-kimi.fast")...)

	got := discRun(t)
	w, ok := discByID(got, "worker@"+discTeamName)
	if !ok || w.State != StateBypassed || w.Provider != "glm" || w.Backend != BackendTmux || w.PaneID != "%3" {
		t.Fatalf("profile+ccf row = %+v (found %v), want bypassed glm in %%3", w, ok)
	}
	fast, ok := discByID(got, "fast@"+discTeamName)
	if !ok || fast.State != StateBypassed || fast.Provider != "kimi" || fast.Backend != BackendUnknown || fast.PaneID != "" {
		t.Fatalf("ccf-only row = %+v (found %v), want bypassed kimi, backend unknown", fast, ok)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
}

// TestDiscoverBypassedInProcess: an active ccf-* member Claude Code runs
// in-process is bypassed with no pid or pane; inactive, pane-backed and native
// members are not.
func TestDiscoverBypassedInProcess(t *testing.T) {
	f := discSetup(t)
	inproc := func(id, typ string, active any) map[string]any {
		m := discMemberRow(id, typ)
		m["backendType"], m["tmuxPaneId"] = "in-process", "in-process"
		if active != nil {
			m["isActive"] = active
		}
		return m
	}
	f.team(discTeamName,
		inproc("glm1@"+discTeamName, "ccf-glm", nil),
		inproc("kimi1@"+discTeamName, "ccf-kimi", false),
		inproc("native@"+discTeamName, "general-purpose", true),
		discMemberRow("qwen1@"+discTeamName, "ccf-qwen"),
	)
	got := discRun(t)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	want := Teammate{
		AgentID: "glm1@" + discTeamName, Name: "glm1", Team: discTeamName, Provider: "glm",
		Backend: BackendInProcess, State: StateBypassed, SpawnTime: discJoinedAt,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("in-process row:\n got %+v\nwant %+v", got[0], want)
	}
}

// TestDiscoverFailedMarker: a dead pane whose capture ends in a launcher
// failure line for a ccf-* member is failed, without argv or profile. A marker
// for a native member (the passthrough CLAUDE_NOT_FOUND) is skipped.
func TestDiscoverFailedMarker(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName,
		discMemberRow("worker@"+discTeamName, "ccf-glm"),
		discMemberRow("native@"+discTeamName, "general-purpose"),
	)
	marker := teammate.FormatFailureLine("PROFILE_WRITE_FAILED", "worker@"+discTeamName, "could not write", "run cc-fleet repair")
	f.deadPane(discSock, "%7", "starting\n"+marker+"\n\n\n")
	native := teammate.FormatFailureLine("CLAUDE_NOT_FOUND", "native@"+discTeamName, "no claude", "install")
	f.deadPane(discSock, "%8", native+"\n")
	stranger := teammate.FormatFailureLine("BAD_ARGS", "ghost@"+discTeamName, "x", "y")
	f.deadPane(discSock, "%9", stranger+"\n")
	f.deadPane(discSock, "%10", "Pane is dead (status 1)\n")

	got := discRun(t)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	want := Teammate{
		AgentID: "worker@" + discTeamName, Name: "worker", Team: discTeamName, PaneID: "%7", Provider: "glm",
		Socket: discSock, Backend: BackendTmux, State: StateFailed, ErrorCode: "PROFILE_WRITE_FAILED",
		SpawnTime: discJoinedAt,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("failed row:\n got %+v\nwant %+v", got[0], want)
	}
	for _, n := range f.capLines {
		if n != markerCaptureLines {
			t.Fatalf("capture lines = %v, want %d each", f.capLines, markerCaptureLines)
		}
	}
}

// TestDiscoverFailedMarkerNarrowPane: in a narrow pane a long marker wraps over
// several physical lines; discovery captures with -J, so the fake tmux (which
// wraps at 40 columns unless -J is given) returns it joined and the full
// 60-character agent id survives.
func TestDiscoverFailedMarkerNarrowPane(t *testing.T) {
	f := discSetup(t)
	captureJoinedFn = f.origCapture // exercise the real tmux.Server.CaptureJoined

	agentID := strings.Repeat("w", 60-len("@"+discTeamName)) + "@" + discTeamName
	if len(agentID) != 60 {
		t.Fatalf("agent id length %d", len(agentID))
	}
	f.team(discTeamName, discMemberRow(agentID, "ccf-glm"))
	marker := teammate.FormatFailureLine("CODEX_PROXY_UNAVAILABLE", agentID, "proxy down", "run cc-fleet codex-proxy status")
	var wrapped strings.Builder
	for rest := marker; rest != ""; {
		n := min(40, len(rest))
		wrapped.WriteString(rest[:n] + "\n")
		rest = rest[n:]
	}

	dir := t.TempDir()
	full, wrap, argsLog := filepath.Join(dir, "full"), filepath.Join(dir, "wrapped"), filepath.Join(dir, "args")
	if err := os.WriteFile(full, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrap, []byte(wrapped.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"out=\"$DISC_WRAPPED\"\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$DISC_ARGS\"; [ \"$a\" = -J ] && out=\"$DISC_FULL\"; done\n" +
		"cat \"$out\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DISC_ARGS", argsLog)
	t.Setenv("DISC_FULL", full)
	t.Setenv("DISC_WRAPPED", wrap)
	f.sockets = []string{discSock}
	f.panes[discSock] = []tmux.PaneInfo{{SocketPath: discSock, SessionName: "main", PaneID: "%5", Dead: true}}

	got := discRun(t)
	if len(got) != 1 || got[0].State != StateFailed || got[0].AgentID != agentID || got[0].ErrorCode != "CODEX_PROXY_UNAVAILABLE" {
		t.Fatalf("rows = %+v, want one failed row for %s", got, agentID)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(strings.Fields(string(data)), " ")
	if want := "-S " + discSock + " capture-pane -p -J -S -200 -t %5"; args != want {
		t.Fatalf("tmux argv = %q, want %q", args, want)
	}
}

// TestDiscoverExcludesRunLead: a `cc-fleet run` lead carries a profile
// --settings but no --agent-id, so it is never a teammate.
func TestDiscoverExcludesRunLead(t *testing.T) {
	f := discSetup(t)
	f.proc(501, discClaude, "--settings", f.profile("glm"), "--model", "glm-4.6")
	f.pane(discSock, "%1", "main", 500, 501)
	if got := discRun(t); len(got) != 0 {
		t.Fatalf("run lead listed: %+v", got)
	}
}

// TestDiscoverIgnoresNativeUnderProviderLead: a native teammate under a
// provider lead inherits the lead's profile --settings; its general-purpose
// agentType keeps it out of the rows, while the provider teammate next to it
// is listed.
func TestDiscoverIgnoresNativeUnderProviderLead(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName,
		discMemberRow("helper@"+discTeamName, "general-purpose"),
		discMemberRow("worker@"+discTeamName, "ccf-glm"),
	)
	f.sessions[discLeadID] = leadsession.Session{PID: 100, SessionID: discLeadID}
	f.proc(501, discArgv("helper@"+discTeamName, f.profile("glm"), "glm-4.6", "--agent-type", "general-purpose")...)
	f.pane(discSock, "%3", "main", 500, 501)
	f.proc(502, discArgv("reviewer@"+discTeamName, f.profile("glm"), "glm-4.6")...) // no member row at all
	f.pane(discSock, "%4", "main", 510, 502)
	f.proc(601, discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6")...)
	f.pane(discSock, "%5", "main", 600, 601)

	got := discRun(t)
	if len(got) != 1 || got[0].AgentID != "worker@"+discTeamName || got[0].State != StateRunning {
		t.Fatalf("rows = %+v, want only the running provider teammate", got)
	}
}

// TestDiscoverUnconfirmedSkipped: without positive evidence a process with a
// profile --settings is skipped — missing config, no member row, a provider
// that disagrees with the profile, or a settings file outside the profiles dir.
func TestDiscoverUnconfirmedSkipped(t *testing.T) {
	cases := []struct {
		name     string
		member   map[string]any // nil = no config at all
		settings func(f *discFixture) string
	}{
		{"config missing", nil, func(f *discFixture) string { return f.profile("glm") }},
		{"no member", discMemberRow("other@"+discTeamName, "ccf-glm"), func(f *discFixture) string { return f.profile("glm") }},
		{"provider mismatch", discMemberRow("worker@"+discTeamName, "ccf-glm"), func(f *discFixture) string { return f.profile("kimi") }},
		{"settings outside profiles", discMemberRow("worker@"+discTeamName, "ccf-glm"), func(*discFixture) string { return "/elsewhere/glm.json" }},
		{"bad agent type", discMemberRow("worker@"+discTeamName, "ccf-glm.bogus"), func(f *discFixture) string { return f.profile("glm") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := discSetup(t)
			if tc.member != nil {
				f.team(discTeamName, tc.member)
			}
			f.proc(501, discArgv("worker@"+discTeamName, tc.settings(f), "glm-4.6")...)
			f.pane(discSock, "%3", "main", 500, 501)
			if got := discRun(t); len(got) != 0 {
				t.Fatalf("unconfirmed process listed: %+v", got)
			}
		})
	}
}

// TestDiscoverLegacySwarm: a 0.3.x teammate on a cc-fleet-swarm-* server is
// attributed by its profile --settings plus that socket, and marked legacy.
func TestDiscoverLegacySwarm(t *testing.T) {
	f := discSetup(t)
	sock := "/tmp/tmux-501/cc-fleet-swarm-alpha"
	f.proc(501, discArgv("w1@alpha", f.profile("deepseek"), "deepseek-chat")...)
	f.pane(sock, "%2", "claude-swarm", 500, 501)

	got := discRun(t)
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(got), got)
	}
	w := got[0]
	if !w.Legacy || w.Provider != "deepseek" || w.Socket != sock || w.Team != "alpha" || w.Name != "w1" || w.Backend != BackendTmux {
		t.Fatalf("legacy row = %+v", w)
	}
	if w.State != StateOrphaned { // no live lead in this fixture
		t.Fatalf("state = %q, want orphaned", w.State)
	}
}

// TestDiscoverLegacyNeedsPositiveEvidence: a non-session team plus a profile
// --settings is not enough; a 0.3.x-only member key (leadSessionId or
// tmuxSocket) is.
func TestDiscoverLegacyNeedsPositiveEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		value any
		want  bool
	}{
		{"no evidence", "", nil, false},
		{"leadSessionId", "leadSessionId", "s-old", true},
		{"tmuxSocket", "tmuxSocket", "cc-fleet-swarm-beta", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := discSetup(t)
			m := discMemberRow("w1@beta", "general-purpose")
			if tc.key != "" {
				m[tc.key] = tc.value
			}
			f.team("beta", m)
			f.proc(501, discArgv("w1@beta", f.profile("glm"), "glm-4.6")...)
			f.pane(discSock, "%3", "main", 500, 501)

			got := discRun(t)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("listed without positive evidence: %+v", got)
				}
				return
			}
			if len(got) != 1 || !got[0].Legacy || got[0].Provider != "glm" || got[0].SpawnTime != discJoinedAt {
				t.Fatalf("rows = %+v, want one legacy glm row", got)
			}
		})
	}
}

// TestDiscoverOrphaned: a running teammate whose --parent-session-id is not a
// live session and whose team has no live lead is orphaned; a team whose lead
// is live under a new session id (after /clear) keeps it running.
func TestDiscoverOrphaned(t *testing.T) {
	f := discSetup(t)
	f.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
	f.proc(501, discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6")...)
	f.pane(discSock, "%3", "main", 500, 501)

	got := discRun(t)
	if len(got) != 1 || got[0].State != StateOrphaned || got[0].LeadPID != 0 || got[0].LeadSessionID != discLeadID {
		t.Fatalf("rows = %+v, want one orphaned row keeping its parent session id", got)
	}

	// The team's lead is live: LeadForTeam matches it by the team config.
	f.live = []leadsession.Session{{PID: 200, SessionID: discLeadID, Cwd: "/work", StartedAt: discJoinedAt}}
	got = discRun(t)
	if len(got) != 1 || got[0].State != StateRunning || got[0].LeadPID != 200 {
		t.Fatalf("rows = %+v, want running with lead pid 200", got)
	}
}

// TestDiscoverResumedParentID: `claude --resume <id>` registers the resumed id,
// so after lead x /clear'd, the live session holding the --parent-session-id of
// x's teammate is the resumer z, which leads its own team: the teammate stays
// x's. When z crashes and z2 resumes the same id, z's teammate is orphaned.
func TestDiscoverResumedParentID(t *testing.T) {
	f := discSetup(t)
	const (
		idA = "aaaaaaaa-0000-4000-8000-000000000001" // x's startup id, resumed by z and z2
		t0  = discJoinedAt
	)
	f.teamOf("session-aaaaaaaa", idA, t0, discMemberRow("wx@session-aaaaaaaa", "ccf-glm"))
	f.teamOf("session-cccccccc", "cccccccc-0000-4000-8000-000000000001", t0+100_000, discMemberRow("wz@session-cccccccc", "ccf-glm"))
	wxArgv := discArgv("wx@session-aaaaaaaa", f.profile("glm"), "glm-4.6")
	wzArgv := discArgv("wz@session-cccccccc", f.profile("glm"), "glm-4.6")
	for _, argv := range [][]string{wxArgv, wzArgv} {
		for i, a := range argv {
			if a == discLeadID {
				argv[i] = idA
			}
		}
	}
	f.proc(501, wxArgv...)
	f.pane(discSock, "%3", "main", 500, 501)
	f.proc(601, wzArgv...)
	f.pane(discSock, "%4", "main", 600, 601)

	x := leadsession.Session{PID: 100, SessionID: "a2d251fb-0000-4000-8000-000000000001", Cwd: "/work", StartedAt: t0 + 300}
	z := leadsession.Session{PID: 200, SessionID: idA, Cwd: "/work", StartedAt: t0 + 100_300}
	f.live = []leadsession.Session{x, z}
	f.sessions[idA] = z
	got := discRun(t)
	for id, want := range map[string]int{"wx@session-aaaaaaaa": 100, "wz@session-cccccccc": 200} {
		if r, ok := discByID(got, id); !ok || r.State != StateRunning || r.LeadPID != want {
			t.Fatalf("%s = %+v, want running with lead pid %d", id, r, want)
		}
	}

	f.teamOf("session-dddddddd", "dddddddd-0000-4000-8000-000000000001", t0+200_000)
	z2 := leadsession.Session{PID: 300, SessionID: idA, Cwd: "/work", StartedAt: t0 + 200_300}
	f.live = []leadsession.Session{x, z2}
	f.sessions[idA] = z2
	got = discRun(t)
	if r, ok := discByID(got, "wx@session-aaaaaaaa"); !ok || r.State != StateRunning || r.LeadPID != 100 {
		t.Fatalf("wx = %+v, want running with lead pid 100", r)
	}
	if r, ok := discByID(got, "wz@session-cccccccc"); !ok || r.State != StateOrphaned || r.LeadPID != 0 {
		t.Fatalf("wz = %+v, want orphaned", r)
	}
}

// TestDiscoverTeammates_NoServer: a stale socket whose server is gone is
// skipped, not an error.
func TestDiscoverTeammates_NoServer(t *testing.T) {
	f := discSetup(t)
	f.sockets = []string{"/tmp/tmux-501/gone"}
	if got := discRun(t); len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestDiscoverTeammates_TmuxNotFound: tmux is not on PATH, with or without
// socket files, and the process table and team configs hold no teammate. A
// missing tmux is not an error (a pure iTerm2 user has none): the result is
// empty and nil, so ps and teardown stay idempotent after the last teammate
// ends. With teammates found elsewhere the rows come back
// (TestFxdiscTmuxNotFoundKeepsRows).
func TestDiscoverTeammates_TmuxNotFound(t *testing.T) {
	for _, sockets := range [][]string{nil, {"/tmp/tmux-501/default"}} {
		f := discSetup(t)
		listPanesFn = f.origListPanes
		f.sockets = sockets
		t.Setenv("PATH", t.TempDir())

		got, err := DiscoverTeammates()
		if err != nil {
			t.Fatalf("sockets %v: DiscoverTeammates with tmux absent: %v, want nil", sockets, err)
		}
		if len(got) != 0 {
			t.Fatalf("sockets %v: expected empty result, got %+v", sockets, got)
		}
	}
}

// TestFx2discTeardownTeamWithoutTmux: with tmux absent and no teammate left,
// tearing a session team down again is a successful no-op, not INTERNAL.
func TestFx2discTeardownTeamWithoutTmux(t *testing.T) {
	f := discSetup(t)
	listPanesFn = f.origListPanes
	f.sockets = []string{"/tmp/tmux-501/default"}
	t.Setenv("PATH", t.TempDir())

	res := Teardown(Target{Kind: TargetTeam, Raw: discTeamName, Team: discTeamName}, nil)
	if !res.OK || res.ErrorCode != "" || len(res.Killed) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want ok with empty killed/skipped", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ok":true,"target":"` + discTeamName + `","killed":[],"skipped":[]}`; string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}

// TestDiscoverTeammates_EmptyServer: a live server with no panes and no
// candidate processes yields a clean empty result.
func TestDiscoverTeammates_EmptyServer(t *testing.T) {
	f := discSetup(t)
	f.sockets = []string{discSock}
	f.panes[discSock] = nil
	if got := discRun(t); len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestDiscoverTeammates_SelfPidNoMatch drives the real process table and child
// walk from a pane whose shell is this test process: nothing in our subtree
// carries --agent-id, so no row lands in that pane.
func TestDiscoverTeammates_SelfPidNoMatch(t *testing.T) {
	f := discSetup(t)
	processTableFn, childrenFn = f.origTable, f.origChildren
	f.pane(discSock, "%1", "main", os.Getpid(), 0)
	got, err := DiscoverTeammates()
	if err != nil {
		t.Fatalf("DiscoverTeammates: %v", err)
	}
	for _, tm := range got {
		if tm.PaneID == "%1" {
			t.Fatalf("a process in our own subtree was listed: %+v", tm)
		}
	}
}

// TestParseDiscArgs: both flag spellings are read and the last occurrence wins.
func TestParseDiscArgs(t *testing.T) {
	a := parseDiscArgs([]string{"claude", "--agent-id", "a@t", "--settings", "/lead.json",
		"--model=first", "--agent-type=ccf-glm", "--settings=/p/glm.json", "--model", "last", "--parent-session-id", "s1"})
	want := discArgs{agentID: "a@t", settings: "/p/glm.json", model: "last", agentType: "ccf-glm", parentSessionID: "s1"}
	if a != want {
		t.Fatalf("parseDiscArgs = %+v, want %+v", a, want)
	}
}

// TestFxdiscTmuxNotFoundKeepsRows: without tmux there is no pane information,
// but the process table and in-process configs are still scanned; rows found
// there come back without an error.
func TestFxdiscTmuxNotFoundKeepsRows(t *testing.T) {
	for _, sockets := range [][]string{nil, {"/tmp/tmux-501/default"}} {
		f := discSetup(t)
		listPanesFn = f.origListPanes
		f.sockets = sockets
		t.Setenv("PATH", t.TempDir())
		m := discMemberRow("glm1@"+discTeamName, "ccf-glm")
		m["backendType"], m["tmuxPaneId"] = "in-process", "in-process"
		f.team(discTeamName, m)
		f.proc(501, discArgv("worker@"+discTeamName, f.profile("glm"), "glm-4.6", "--agent-type", "ccf-glm")...)

		got, err := DiscoverTeammates()
		if err != nil {
			t.Fatalf("sockets %v: DiscoverTeammates with tmux absent and teammates present: %v", sockets, err)
		}
		w, ok := discByID(got, "worker@"+discTeamName)
		if !ok || w.State != StateBypassed || w.Provider != "glm" || w.Backend != BackendUnknown || w.PaneID != "" {
			t.Fatalf("sockets %v: argv row = %+v (found %v), want bypassed glm without a pane", sockets, w, ok)
		}
		in, ok := discByID(got, "glm1@"+discTeamName)
		if !ok || in.State != StateBypassed || in.Backend != BackendInProcess {
			t.Fatalf("sockets %v: in-process row = %+v (found %v), want bypassed in-process", sockets, in, ok)
		}
		if len(got) != 2 {
			t.Fatalf("sockets %v: got %d rows, want 2: %+v", sockets, len(got), got)
		}
	}
}

// TestFxdiscLegacyRawTeamDir: 0.3.x kept a team's config under its raw name
// (teams/My_Team), not Claude Code's sanitized directory; a member key found
// there is still the positive evidence that makes the row legacy.
func TestFxdiscLegacyRawTeamDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		want bool
	}{
		{"no evidence", "", false},
		{"tmuxSocket", "tmuxSocket", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := discSetup(t)
			const team = "My_Team"
			if dir := teammate.SanitizeTeamDir(team); dir == team {
				t.Fatalf("SanitizeTeamDir(%q) = %q, want a different directory", team, dir)
			}
			m := discMemberRow("w1@"+team, "general-purpose")
			if tc.key != "" {
				m[tc.key] = "cc-fleet-swarm-my-team"
			}
			f.team(team, m)
			f.proc(501, discArgv("w1@"+team, f.profile("glm"), "glm-4.6")...)
			f.pane(discSock, "%3", "main", 500, 501)

			got := discRun(t)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("listed without positive evidence: %+v", got)
				}
				return
			}
			if len(got) != 1 || !got[0].Legacy || got[0].Provider != "glm" || got[0].Team != team || got[0].SpawnTime != discJoinedAt {
				t.Fatalf("rows = %+v, want one legacy glm row for %s", got, team)
			}
		})
	}
}
