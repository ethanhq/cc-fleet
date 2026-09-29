//go:build !windows

package teardown

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/teammate"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// killHarness runs the real DiscoverTeammates over a discFixture and fakes
// every kill seam: kill-pane, signals, argv and start-token re-reads. The
// afterDiscover hook lets a test change the world between discovery and the
// identity re-check.
type killHarness struct {
	*discFixture
	start         map[int]string // current start token; absent = "start-<pid>", "" = gone
	afterDiscover func()
	discovered    bool
	tmuxAfter     []string // tmux seam calls made after discovery
	killedPanes   []string // "<socket> <pane>"
	signals       []killSig
	killPaneErr   error
}

type killSig struct {
	pid int
	sig syscall.Signal
}

func killSetup(t *testing.T) *killHarness {
	t.Helper()
	h := &killHarness{discFixture: discSetup(t), start: map[int]string{}}
	origDiscover, origCmdline, origKill := discoverFn, cmdlineFn, killPaneFn
	origSignal, origGrace := signalProc, procReapGrace
	t.Cleanup(func() {
		discoverFn, cmdlineFn, killPaneFn = origDiscover, origCmdline, origKill
		signalProc, procReapGrace = origSignal, origGrace
	})

	discoverFn = func() ([]Teammate, error) {
		ts, err := DiscoverTeammates()
		h.discovered = true
		if h.afterDiscover != nil {
			h.afterDiscover()
		}
		return ts, err
	}
	list, capture := listPanesFn, captureJoinedFn
	listPanesFn = func(sock string) ([]tmux.PaneInfo, error) {
		if h.discovered {
			h.tmuxAfter = append(h.tmuxAfter, "list-panes "+sock)
		}
		return list(sock)
	}
	captureJoinedFn = func(sock, pane string, lines int) (string, error) {
		if h.discovered {
			h.tmuxAfter = append(h.tmuxAfter, "capture-pane "+sock+" "+pane)
		}
		return capture(sock, pane, lines)
	}
	killPaneFn = func(sock, pane string) error {
		h.tmuxAfter = append(h.tmuxAfter, "kill-pane "+sock+" "+pane)
		if h.killPaneErr != nil {
			return h.killPaneErr
		}
		h.killedPanes = append(h.killedPanes, sock+" "+pane)
		return nil
	}
	cmdlineFn = func(pid int) ([]string, error) {
		for _, p := range h.procs {
			if p.PID == pid {
				return p.Argv, nil
			}
		}
		return nil, errors.New("no such process")
	}
	procStartFn = func(pid int) (string, bool) {
		if s, ok := h.start[pid]; ok {
			return s, s != ""
		}
		return "start-" + strconv.Itoa(pid), true
	}
	signalProc = func(pid int, sig syscall.Signal) error {
		h.signals = append(h.signals, killSig{pid, sig})
		return nil
	}
	procReapGrace = 0
	return h
}

// setArgv replaces pid's current argv.
func (h *killHarness) setArgv(pid int, argv []string) {
	for i := range h.procs {
		if h.procs[i].PID == pid {
			h.procs[i].Argv = argv
		}
	}
}

func (h *killHarness) signalledPIDs() []int {
	var pids []int
	for _, s := range h.signals {
		if !slices.Contains(pids, s.pid) {
			pids = append(pids, s.pid)
		}
	}
	return pids
}

func killRun(t *testing.T, arg, socket string) Result {
	t.Helper()
	tg, err := ParseTarget(arg, socket)
	if err != nil {
		t.Fatalf("ParseTarget(%q): %v", arg, err)
	}
	return Teardown(tg, nil)
}

// killProviderTeam seeds a session team whose lead is live, with a provider
// teammate worker (pid 601 in pane %5) and a native helper that inherited the
// provider lead's profile --settings (pid 501 in pane %3).
func killProviderTeam(h *killHarness) {
	h.team(discTeamName,
		discMemberRow("helper@"+discTeamName, "general-purpose"),
		discMemberRow("worker@"+discTeamName, "ccf-glm"),
	)
	h.sessions[discLeadID] = leadsession.Session{PID: 100, SessionID: discLeadID}
	h.proc(100, discClaude, "--settings", h.profile("glm"), "--model", "glm-4.6") // the provider lead
	h.pane(discSock, "%1", "main", 99, 100)
	h.proc(501, discArgv("helper@"+discTeamName, h.profile("glm"), "glm-4.6", "--agent-type", "general-purpose")...)
	h.pane(discSock, "%3", "main", 500, 501)
	h.proc(601, discArgv("worker@"+discTeamName, h.profile("glm"), "glm-4.6")...)
	h.pane(discSock, "%5", "main", 600, 601)
}

// TestTeardownTeamSparesNativeUnderProviderLead: `teardown <team>` kills the
// provider teammate only; the native teammate carrying the inherited profile
// --settings is never a candidate.
func TestTeardownTeamSparesNativeUnderProviderLead(t *testing.T) {
	h := killSetup(t)
	killProviderTeam(h)

	res := killRun(t, discTeamName, "")
	want := []Killed{{AgentID: "worker@" + discTeamName, PaneID: "%5", Socket: discSock, PID: 601}}
	if !res.OK || !reflect.DeepEqual(res.Killed, want) || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want only worker killed", res)
	}
	if !reflect.DeepEqual(h.killedPanes, []string{discSock + " %5"}) {
		t.Fatalf("killed panes = %v, want only %%5", h.killedPanes)
	}
	if got := h.signalledPIDs(); !reflect.DeepEqual(got, []int{601}) {
		t.Fatalf("signalled pids = %v, want [601]", got)
	}
}

// TestTeardownTeamKeepsTeamDir: teardown only reads the team config; the team
// directory and its files are unchanged afterwards.
func TestTeardownTeamKeepsTeamDir(t *testing.T) {
	h := killSetup(t)
	killProviderTeam(h)
	teams := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "teams")
	before := killSnapshot(t, teams)

	if res := killRun(t, discTeamName, ""); !res.OK || len(res.Killed) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if after := killSnapshot(t, teams); !reflect.DeepEqual(before, after) {
		t.Fatalf("teams dir changed:\nbefore %v\nafter  %v", before, after)
	}
}

// killSnapshot maps every path under root to its contents.
func killSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			snap[path] = "<dir>"
			return nil
		}
		data, err := os.ReadFile(path)
		snap[path] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestTeardownNeverTouchesLeaderRow: the lead's config row (tmuxPaneId
// "leader") and the lead process in its pane are never killed, whether the
// target is the team or the lead's own pane.
func TestTeardownNeverTouchesLeaderRow(t *testing.T) {
	h := killSetup(t)
	killProviderTeam(h)

	for _, arg := range []string{discTeamName, "%1", "leader", "team-lead@" + discTeamName} {
		h.killedPanes, h.signals = nil, nil
		tg, err := ParseTarget(arg, "")
		if err != nil {
			continue // "leader" is not a target at all
		}
		res := Teardown(tg, nil)
		for _, k := range res.Killed {
			if k.AgentID == "team-lead@"+discTeamName || k.PaneID == "%1" || k.PID == 100 {
				t.Fatalf("%s: lead killed: %+v", arg, res)
			}
		}
		for _, p := range h.killedPanes {
			if strings.HasSuffix(p, " %1") || strings.HasSuffix(p, " leader") {
				t.Fatalf("%s: lead pane killed: %v", arg, h.killedPanes)
			}
		}
		if slices.Contains(h.signalledPIDs(), 100) {
			t.Fatalf("%s: lead pid signalled", arg)
		}
	}
}

// TestTeardownAmbiguousPane: the same %N on two servers needs --socket; with
// it only that server's teammate is killed.
func TestTeardownAmbiguousPane(t *testing.T) {
	h := killSetup(t)
	other := "/tmp/tmux-501/work"
	h.team(discTeamName,
		discMemberRow("a@"+discTeamName, "ccf-glm"),
		discMemberRow("b@"+discTeamName, "ccf-glm"),
	)
	h.proc(601, discArgv("a@"+discTeamName, h.profile("glm"), "glm-4.6")...)
	h.pane(discSock, "%3", "main", 600, 601)
	h.proc(701, discArgv("b@"+discTeamName, h.profile("glm"), "glm-4.6")...)
	h.pane(other, "%3", "main", 700, 701)

	res := killRun(t, "%3", "")
	if res.OK || res.ErrorCode != ErrCodeAmbiguousTarget ||
		!strings.Contains(res.ErrorMsg, discSock) || !strings.Contains(res.ErrorMsg, other) {
		t.Fatalf("result = %+v, want AMBIGUOUS_TARGET naming both sockets", res)
	}
	if len(h.killedPanes) != 0 || len(h.signals) != 0 {
		t.Fatalf("ambiguous target acted: panes %v signals %v", h.killedPanes, h.signals)
	}

	res = killRun(t, "%3", other)
	if !res.OK || len(res.Killed) != 1 || res.Killed[0].AgentID != "b@"+discTeamName {
		t.Fatalf("with --socket: %+v", res)
	}
	if !reflect.DeepEqual(h.killedPanes, []string{other + " %3"}) {
		t.Fatalf("killed panes = %v", h.killedPanes)
	}
}

// TestTeardownIdentityMismatch: a process whose argv changed after discovery
// (or that left its pane) is skipped: no kill-pane, no signal.
func TestTeardownIdentityMismatch(t *testing.T) {
	cases := []struct {
		name   string
		inPane bool
		change func(h *killHarness)
	}{
		{"tmux argv changed", true, func(h *killHarness) { h.setArgv(601, []string{"/usr/bin/vim"}) }},
		{"tmux left pane", true, func(h *killHarness) { h.children[600] = nil }},
		{"pid argv changed", false, func(h *killHarness) { h.setArgv(601, []string{"/usr/bin/vim"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := killSetup(t)
			h.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
			h.proc(601, discArgv("worker@"+discTeamName, h.profile("glm"), "glm-4.6")...)
			if tc.inPane {
				h.pane(discSock, "%5", "main", 600, 601)
			}
			h.afterDiscover = func() { tc.change(h) }

			res := killRun(t, "worker@"+discTeamName, "")
			if !res.OK || len(res.Killed) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != ReasonIdentityMismatch {
				t.Fatalf("result = %+v, want one IDENTITY_MISMATCH skip", res)
			}
			if len(h.killedPanes) != 0 || len(h.signals) != 0 {
				t.Fatalf("acted on a mismatch: panes %v signals %v", h.killedPanes, h.signals)
			}
		})
	}
}

// TestTeardownPIDBranchNoPane: a teammate outside any tmux pane (backend
// unknown, e.g. an iTerm2 split) is reaped by pid after re-verification, with
// no tmux command at all.
func TestTeardownPIDBranchNoPane(t *testing.T) {
	h := killSetup(t)
	h.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
	h.proc(601, discArgv("worker@"+discTeamName, h.profile("glm"), "glm-4.6")...)

	res := killRun(t, "worker@"+discTeamName, "")
	want := []Killed{{AgentID: "worker@" + discTeamName, PID: 601}}
	if !res.OK || !reflect.DeepEqual(res.Killed, want) {
		t.Fatalf("result = %+v, want pid-only kill", res)
	}
	if len(h.tmuxAfter) != 0 {
		t.Fatalf("tmux touched in the pid branch: %v", h.tmuxAfter)
	}
	wantSigs := []killSig{{601, syscall.SIGTERM}, {601, syscall.SIGKILL}}
	if !reflect.DeepEqual(h.signals, wantSigs) {
		t.Fatalf("signals = %v, want %v", h.signals, wantSigs)
	}
}

// TestTeardownPIDReusedSkips: the pid now carries another start token (it
// exited and the kernel reused it), so nothing is signalled.
func TestTeardownPIDReusedSkips(t *testing.T) {
	for _, inPane := range []bool{false, true} {
		h := killSetup(t)
		h.team(discTeamName, discMemberRow("worker@"+discTeamName, "ccf-glm"))
		h.proc(601, discArgv("worker@"+discTeamName, h.profile("glm"), "glm-4.6")...)
		if inPane {
			h.pane(discSock, "%5", "main", 600, 601)
		}
		h.afterDiscover = func() { h.start[601] = "start-reused" }

		res := killRun(t, "worker@"+discTeamName, "")
		if !res.OK || len(res.Killed) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != ReasonIdentityMismatch {
			t.Fatalf("inPane=%v: result = %+v, want IDENTITY_MISMATCH", inPane, res)
		}
		if len(h.signals) != 0 || len(h.killedPanes) != 0 {
			t.Fatalf("inPane=%v: signals %v panes %v", inPane, h.signals, h.killedPanes)
		}
	}
}

// TestTeardownInProcessSkipped: an in-process bypassed teammate cannot be
// killed from outside the lead.
func TestTeardownInProcessSkipped(t *testing.T) {
	h := killSetup(t)
	m := discMemberRow("worker@"+discTeamName, "ccf-glm")
	m["backendType"] = "in-process"
	h.team(discTeamName, m)

	res := killRun(t, discTeamName, "")
	if !res.OK || len(res.Killed) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != ReasonInProcess ||
		!strings.Contains(res.Suggestion, "TaskStop") {
		t.Fatalf("result = %+v, want IN_PROCESS skip", res)
	}
	if len(h.tmuxAfter) != 0 || len(h.signals) != 0 {
		t.Fatalf("acted on in-process: %v %v", h.tmuxAfter, h.signals)
	}
}

// TestTeardownFailedPane: a failed dead pane is killed only while its marker
// still names the same agent.
func TestTeardownFailedPane(t *testing.T) {
	agentID := "worker@" + discTeamName
	marker := teammate.FormatFailureLine("CLAUDE_NOT_FOUND", agentID, "no claude", "install it")
	for _, changed := range []bool{false, true} {
		h := killSetup(t)
		h.team(discTeamName, discMemberRow(agentID, "ccf-glm"))
		h.deadPane(discSock, "%7", "boot\n"+marker+"\n")
		if changed {
			h.afterDiscover = func() { h.captures[discSock+" %7"] = "something else\n" }
		}

		res := killRun(t, "%7", "")
		if changed {
			if len(res.Killed) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != ReasonIdentityMismatch || len(h.killedPanes) != 0 {
				t.Fatalf("changed marker: %+v panes %v", res, h.killedPanes)
			}
			continue
		}
		want := []Killed{{AgentID: agentID, PaneID: "%7", Socket: discSock}}
		if !res.OK || !reflect.DeepEqual(res.Killed, want) || len(h.signals) != 0 {
			t.Fatalf("result = %+v signals %v", res, h.signals)
		}
	}
}

// TestTeardownNothingToKill: no candidate is ok with empty killed/skipped
// lists that encode as [] (not null).
func TestTeardownNothingToKill(t *testing.T) {
	killSetup(t)
	res := killRun(t, "%9", "")
	if !res.OK || res.ErrorCode != "" {
		t.Fatalf("result = %+v", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ok":true,"target":"%9","killed":[],"skipped":[]}`; string(data) != want {
		t.Fatalf("json = %s, want %s", data, want)
	}
}

// TestTeardownKillPaneFailure: a kill-pane error fails the result instead of
// reporting the teammate as killed.
func TestTeardownKillPaneFailure(t *testing.T) {
	h := killSetup(t)
	killProviderTeam(h)
	h.killPaneErr = errors.New("tmux kill-pane %5: boom")
	res := killRun(t, "worker@"+discTeamName, "")
	if res.OK || res.ErrorCode != ErrCodeInternal || len(res.Killed) != 0 || !strings.Contains(res.ErrorMsg, "boom") {
		t.Fatalf("result = %+v", res)
	}
}
