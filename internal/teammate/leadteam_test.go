package teammate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
)

// procStartedAt is the lead's startedAt (unix ms) shared by the leadteam tests.
const procStartedAt int64 = 1_790_000_000_000

// procWriteTeam writes teams/<name>/config.json in Claude Code's native shape:
// a team-lead row plus one ordinary teammate row with a different cwd. leadCwd
// is slash-separated and stored with the OS separator, as Claude Code records it.
func procWriteTeam(t *testing.T, name, leadSessionID, leadCwd string, createdAt int64) string {
	t.Helper()
	dir := filepath.Join(claudepaths.Teams(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"name":          name,
		"createdAt":     createdAt,
		"leadAgentId":   "team-lead@" + name,
		"leadSessionId": leadSessionID,
		"members": []map[string]any{
			{"agentId": "worker@" + name, "name": "worker", "cwd": "/elsewhere", "tmuxPaneId": "%3", "backendType": "tmux"},
			{"agentId": "team-lead@" + name, "name": "team-lead", "cwd": filepath.FromSlash(leadCwd), "joinedAt": createdAt, "tmuxPaneId": "leader", "backendType": "in-process"},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// procSession is a terminal lead session whose process started a second
// before it registered; ProcStart holds that start in unix ms (see
// procFakeStart). cwd is slash-separated, like procWriteTeam's leadCwd.
func procSession(id, cwd string, startedAt int64) leadsession.Session {
	return leadsession.Session{PID: 4242, SessionID: id, Cwd: filepath.FromSlash(cwd), StartedAt: startedAt, ProcStart: strconv.FormatInt(startedAt-1_000, 10), Version: "2.1.281", Kind: "interactive", Entrypoint: "cli"}
}

// procFakeStart reads a session's process start from the unix ms in ProcStart,
// floored to the whole second like the real procStart. The start does not
// follow wall-clock steps (macOS) unless the test says so (procDrifts).
func procFakeStart(t *testing.T) {
	orig, origDrifts := leadStartMsFn, leadStartDrifts
	leadStartMsFn = func(s leadsession.Session) (int64, bool) {
		ms, err := strconv.ParseInt(s.ProcStart, 10, 64)
		return ms / 1000 * 1000, err == nil
	}
	leadStartDrifts = false
	t.Cleanup(func() { leadStartMsFn, leadStartDrifts = orig, origDrifts })
}

// procDrifts makes the process start follow wall-clock steps, as on Linux.
func procDrifts() { leadStartDrifts = true }

// procRegistry fakes the sessions/ registry.
func procRegistry(t *testing.T, ss ...leadsession.Session) {
	orig := registryFn
	registryFn = func() []leadsession.Session { return ss }
	t.Cleanup(func() { registryFn = orig })
}

func TestFindLeadTeamDirect(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	s := procSession("a1b2c3d4-0000-4000-8000-000000000001", "/work/p", procStartedAt)
	procRegistry(t, s)
	dir := procWriteTeam(t, "session-a1b2c3d4", s.SessionID, s.Cwd, procStartedAt-200)
	// A later fallback candidate must not beat the direct hit.
	procWriteTeam(t, "session-99999999", "99999999-0000-4000-8000-000000000009", s.Cwd, procStartedAt-100)

	got, ok := FindLeadTeam(s)
	want := LeadTeam{Name: "session-a1b2c3d4", Dir: dir, LeadSessionID: s.SessionID, LeadCwd: s.Cwd, CreatedAt: procStartedAt - 200}
	if !ok || got != want {
		t.Fatalf("FindLeadTeam = %+v, %v; want %+v", got, ok, want)
	}

	// The direct candidate must also carry our session id: a prefix-only
	// collision falls through to the cwd/createdAt scan.
	other := procSession("a1b2c3d4-ffff-4000-8000-00000000000f", "/work/none", procStartedAt)
	if got, ok := FindLeadTeam(other); ok {
		t.Fatalf("FindLeadTeam(prefix collision) = %+v, want !ok", got)
	}
}

// TestFindLeadTeamAfterClear: /clear changed the session id, so the team is
// found by the lead row's cwd and createdAt. Claude Code creates the team
// before a startup dialog and before it registers the session (2.1.281 with
// the trust dialog open 31s: createdAt 31137ms before startedAt), so the
// latest team created between the process start and startedAt wins: a lead
// started 30s later in the same cwd created its team after this one
// registered, and a lead that still has its session id owns its team.
func TestFindLeadTeamAfterClear(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	s := procSession("ffff0000-0000-4000-8000-000000000002", "/work/p", procStartedAt)
	s.ProcStart = strconv.FormatInt(procStartedAt-60_500, 10) // a startup dialog stayed open a minute
	s2 := procSession("eeee0000-0000-4000-8000-000000000004", s.Cwd, procStartedAt+30_250)
	busy := procSession("c1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt-29_000) // started during s's dialog, no /clear
	procRegistry(t, s, s2, busy)
	own := procWriteTeam(t, "session-b1b2c3d4", "b1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt-60_000)
	later := procWriteTeam(t, "session-a1b2c3d4", "a1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt+29_800)
	procWriteTeam(t, "session-9a9a9a9a", "9a9a9a9a-0000-4000-8000-000000000001", s.Cwd, procStartedAt+5_000)       // after s registered, its lead left no sessions file
	procWriteTeam(t, "session-c1b2c3d4", busy.SessionID, s.Cwd, procStartedAt-30_000)                              // busy's own team
	procWriteTeam(t, "session-d1b2c3d4", "d1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt-61_001)      // before the process started
	procWriteTeam(t, "session-e1b2c3d4", "e1b2c3d4-0000-4000-8000-000000000001", "/work/other", procStartedAt-100) // other cwd
	procWriteTeam(t, "my-team", "f1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt-100)                  // not a session team

	got, ok := FindLeadTeam(s)
	if !ok || got.Name != "session-b1b2c3d4" || got.Dir != own || got.LeadCwd != s.Cwd || got.CreatedAt != procStartedAt-60_000 {
		t.Fatalf("FindLeadTeam = %+v, %v; want session-b1b2c3d4 (created during startup)", got, ok)
	}
	// The lead that started 30s later, also after /clear, lands on its own team.
	if got, ok := FindLeadTeam(s2); !ok || got.Dir != later {
		t.Fatalf("FindLeadTeam(later lead) = %+v, %v; want session-a1b2c3d4", got, ok)
	}
	for _, live := range [][]leadsession.Session{{s, s2, busy}, {busy, s2, s}} {
		for team, want := range map[string]leadsession.Session{"session-b1b2c3d4": s, "session-a1b2c3d4": s2, "session-c1b2c3d4": busy} {
			if got, ok := LeadForTeam(team, live); !ok || got != want {
				t.Fatalf("LeadForTeam(%s) = %+v, %v; want %+v", team, got, ok, want)
			}
		}
	}

	if got, ok := FindLeadTeam(procSession(s.SessionID, "/work/nothing-here", procStartedAt)); ok {
		t.Fatalf("FindLeadTeam(no cwd match) = %+v, want !ok", got)
	}
	// Without the process start only leadTeamSkewMs before startedAt is searched.
	noStart := s
	noStart.ProcStart = ""
	if got, ok := FindLeadTeam(noStart); ok {
		t.Fatalf("FindLeadTeam(no process start, team 60s early) = %+v, want !ok", got)
	}
}

// TestFindLeadTeamCloseStarts: two leads started in the same second in one
// cwd (2.1.281, live: teams 24ms apart, the second created before the first
// registered) each keep their own team after /clear, whether or not the other
// lead cleared too.
func TestFindLeadTeamCloseStarts(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	l1 := procSession("11110000-0000-4000-8000-000000000001", "/work/p", procStartedAt+650)
	l1.ProcStart = strconv.FormatInt(procStartedAt, 10)
	l2 := procSession("22220000-0000-4000-8000-000000000002", "/work/p", procStartedAt+683)
	l2.ProcStart = strconv.FormatInt(procStartedAt, 10)
	t1 := procWriteTeam(t, "session-436df5c9", "436df5c9-0000-4000-8000-000000000001", "/work/p", procStartedAt+559)
	t2 := procWriteTeam(t, "session-a28164fe", "a28164fe-0000-4000-8000-000000000002", "/work/p", procStartedAt+583)

	l1Kept := l1 // l1 before its /clear
	l1Kept.SessionID = "436df5c9-0000-4000-8000-000000000001"
	l2Kept := l2
	l2Kept.SessionID = "a28164fe-0000-4000-8000-000000000002"
	for _, tc := range []struct {
		name   string
		live   []leadsession.Session
		t1, t2 bool // whether l1 / l2 resolve through the fallback
	}{
		{"both cleared", []leadsession.Session{l1, l2}, true, true},
		{"only l1 cleared", []leadsession.Session{l1, l2Kept}, true, false},
		{"only l2 cleared", []leadsession.Session{l1Kept, l2}, false, true},
	} {
		procRegistry(t, tc.live...)
		if tc.t1 {
			if got, ok := FindLeadTeam(l1); !ok || got.Dir != t1 {
				t.Fatalf("%s: FindLeadTeam(l1) = %+v, %v; want %s", tc.name, got, ok, t1)
			}
		}
		if tc.t2 {
			if got, ok := FindLeadTeam(l2); !ok || got.Dir != t2 {
				t.Fatalf("%s: FindLeadTeam(l2) = %+v, %v; want %s", tc.name, got, ok, t2)
			}
		}
		for _, live := range [][]leadsession.Session{tc.live, {tc.live[1], tc.live[0]}} {
			if got, ok := LeadForTeam("session-436df5c9", live); !ok || got != tc.live[0] {
				t.Fatalf("%s: LeadForTeam(l1's team) = %+v, %v; want l1", tc.name, got, ok)
			}
			if got, ok := LeadForTeam("session-a28164fe", live); !ok || got != tc.live[1] {
				t.Fatalf("%s: LeadForTeam(l2's team) = %+v, %v; want l2", tc.name, got, ok)
			}
		}
	}
}

// TestFindLeadTeamThreeCloseStarts: three leads in one cwd, the third started a
// second later, all /clear'd. The latest to register claims first; claiming in
// registration order instead would hand the third lead's team to the second
// and the second's to the first.
func TestFindLeadTeamThreeCloseStarts(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	l1 := procSession("11110000-0000-4000-8000-000000000001", "/work/p", procStartedAt+1_650)
	l1.ProcStart = strconv.FormatInt(procStartedAt, 10)
	l2 := procSession("22220000-0000-4000-8000-000000000002", "/work/p", procStartedAt+1_683)
	l2.ProcStart = strconv.FormatInt(procStartedAt, 10)
	l3 := procSession("33330000-0000-4000-8000-000000000003", "/work/p", procStartedAt+1_700)
	l3.ProcStart = strconv.FormatInt(procStartedAt+1_000, 10)
	procRegistry(t, l1, l2, l3)
	want := map[string]leadsession.Session{
		procWriteTeam(t, "session-11111111", "11111111-0000-4000-8000-000000000001", "/work/p", procStartedAt+559):   l1,
		procWriteTeam(t, "session-22222222", "22222222-0000-4000-8000-000000000002", "/work/p", procStartedAt+583):   l2,
		procWriteTeam(t, "session-33333333", "33333333-0000-4000-8000-000000000003", "/work/p", procStartedAt+1_200): l3,
	}
	for dir, s := range want {
		if got, ok := FindLeadTeam(s); !ok || got.Dir != dir {
			t.Fatalf("FindLeadTeam(%s) = %+v, %v; want %s", s.SessionID[:4], got, ok, dir)
		}
		if got, ok := LeadForTeam(filepath.Base(dir), []leadsession.Session{l3, l1, l2}); !ok || got != s {
			t.Fatalf("LeadForTeam(%s) = %+v, %v; want %s", filepath.Base(dir), got, ok, s.SessionID[:4])
		}
	}
}

// TestFindLeadTeamTie: two leads whose teams were created in the same
// millisecond (2.1.281, live: started back to back) cannot be told apart after
// both /clear. FindLeadTeam reports no team rather than a guess; LeadForTeam
// still sees both teams' leads alive. A third lead a second later is unaffected.
func TestFindLeadTeamTie(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	c1 := procSession("c1c10000-0000-4000-8000-000000000001", "/work/p", procStartedAt+1_375)
	c1.ProcStart = strconv.FormatInt(procStartedAt, 10)
	c2 := procSession("c2c20000-0000-4000-8000-000000000002", "/work/p", procStartedAt+1_373)
	c2.ProcStart = strconv.FormatInt(procStartedAt, 10)
	c3 := procSession("c3c30000-0000-4000-8000-000000000003", "/work/p", procStartedAt+1_994)
	c3.ProcStart = strconv.FormatInt(procStartedAt+1_000, 10)
	c1.PID, c2.PID, c3.PID = 56022, 56034, 56147
	procRegistry(t, c1, c2, c3)
	procWriteTeam(t, "session-de9daa6b", "de9daa6b-0000-4000-8000-000000000001", "/work/p", procStartedAt+1_283)
	procWriteTeam(t, "session-15ab6fb4", "15ab6fb4-0000-4000-8000-000000000002", "/work/p", procStartedAt+1_283)
	t3 := procWriteTeam(t, "session-a416721f", "a416721f-0000-4000-8000-000000000003", "/work/p", procStartedAt+1_910)

	for _, s := range []leadsession.Session{c1, c2} {
		if got, ok := FindLeadTeam(s); ok {
			t.Fatalf("FindLeadTeam(%s) = %+v, want !ok (tie)", s.SessionID[:4], got)
		}
	}
	if got, ok := FindLeadTeam(c3); !ok || got.Dir != t3 {
		t.Fatalf("FindLeadTeam(c3) = %+v, %v; want %s", got, ok, t3)
	}
	live := []leadsession.Session{c1, c2, c3}
	a, okA := LeadForTeam("session-de9daa6b", live)
	b, okB := LeadForTeam("session-15ab6fb4", live)
	if !okA || !okB || a == b || (a != c1 && a != c2) || (b != c1 && b != c2) {
		t.Fatalf("LeadForTeam(tied teams) = %+v %v, %+v %v; want c1 and c2, one each", a, okA, b, okB)
	}
	if got, ok := LeadForTeam("session-a416721f", live); !ok || got != c3 {
		t.Fatalf("LeadForTeam(c3's team) = %+v, %v; want c3", got, ok)
	}
}

// TestFindLeadTeamEarlyProcessStart: a claude exec'd from an old shell keeps
// the shell's process start, so older teams in the cwd are in range; the
// lead's own team is still the latest one before it registered.
func TestFindLeadTeamEarlyProcessStart(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	s := procSession("ffff0000-0000-4000-8000-000000000003", "/work/p", procStartedAt)
	s.ProcStart = strconv.FormatInt(procStartedAt-3_600_000, 10)
	procRegistry(t, s)
	procWriteTeam(t, "session-0ld00001", "0ld00001-0000-4000-8000-000000000001", s.Cwd, procStartedAt-1_800_000) // a crashed lead's
	own := procWriteTeam(t, "session-b1b2c3d4", "b1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt-250)
	if got, ok := FindLeadTeam(s); !ok || got.Dir != own {
		t.Fatalf("FindLeadTeam = %+v, %v; want %s", got, ok, own)
	}
}

// TestFindLeadTeamTeamlessSession: a session without a team of its own (the
// desktop app, -p, SDK, or a terminal with agent teams off) opened in the same
// cwd seconds after a lead cannot claim that lead's team once the lead
// /clear'd or crashed, whatever its entrypoint: the team predates its process.
func TestFindLeadTeamTeamlessSession(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	procRegistry(t)
	procWriteTeam(t, "session-a1b2c3d4", "a1b2c3d4-0000-4000-8000-000000000001", "/work/p", procStartedAt-250)
	for _, ep := range []string{"claude-desktop", "sdk-cli", "sdk-ts", "cli"} {
		s := procSession("dddd0000-0000-4000-8000-000000000005", "/work/p", procStartedAt+3_000)
		s.Entrypoint = ep
		if got, ok := FindLeadTeam(s); ok {
			t.Fatalf("FindLeadTeam(%s session 3s later) = %+v, want !ok", ep, got)
		}
		if got, ok := LeadForTeam("session-a1b2c3d4", []leadsession.Session{s}); ok {
			t.Fatalf("LeadForTeam(dead lead's team, live=[%s session 3s later]) = %+v, want !ok (orphaned)", ep, got)
		}
	}
}

// TestFindLeadTeamInheritedEntrypoint: Claude Code keeps an inherited
// CLAUDE_CODE_ENTRYPOINT, so a terminal lead in a tmux server started from a
// desktop session registers entrypoint claude-desktop; it still has a team and
// keeps it after /clear.
func TestFindLeadTeamInheritedEntrypoint(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	s := procSession("e0e00000-0000-4000-8000-000000000001", "/work/p", procStartedAt)
	s.Entrypoint = "claude-desktop"
	procRegistry(t, s)
	own := procWriteTeam(t, "session-c35315cf", "c35315cf-0000-4000-8000-000000000001", s.Cwd, procStartedAt-79)
	if got, ok := FindLeadTeam(s); !ok || got.Dir != own {
		t.Fatalf("FindLeadTeam = %+v, %v; want %s", got, ok, own)
	}
}

// TestFindLeadTeamResume: --resume and --continue register the resumed id, but
// Claude Code named the new process's team after its fresh startup id. The
// resumed id's team, if its lead is still alive (/clear'd since) or crashed,
// predates the resumer and is not its team; the /clear'd lead keeps it.
func TestFindLeadTeamResume(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	x := procSession("84a7d10f-0000-4000-8000-000000000001", "/work/p", procStartedAt) // /clear'd, was 106024d3…
	x.PID = 100
	z := procSession("106024d3-0000-4000-8000-000000000001", "/work/p", procStartedAt+206_000) // claude --resume 106024d3…
	z.PID = 200
	t0 := procWriteTeam(t, "session-106024d3", z.SessionID, "/work/p", procStartedAt-194)
	tz := procWriteTeam(t, "session-ccc3716f", "ccc3716f-0000-4000-8000-000000000001", "/work/p", procStartedAt+205_911)

	procRegistry(t, x, z)
	for s, want := range map[*leadsession.Session]string{&x: t0, &z: tz} {
		if got, ok := FindLeadTeam(*s); !ok || got.Dir != want {
			t.Fatalf("FindLeadTeam(pid %d) = %+v, %v; want %s", s.PID, got, ok, want)
		}
	}
	for _, live := range [][]leadsession.Session{{x, z}, {z, x}} {
		if got, ok := LeadForTeam("session-106024d3", live); !ok || got != x {
			t.Fatalf("LeadForTeam(session-106024d3) = %+v, %v; want the /clear'd lead", got, ok)
		}
		if got, ok := LeadForTeam("session-ccc3716f", live); !ok || got != z {
			t.Fatalf("LeadForTeam(session-ccc3716f) = %+v, %v; want the resumed lead", got, ok)
		}
	}

	// The resumed id's lead crashed and left its team: the resumer still gets
	// its own team, and the dead lead's team is orphaned.
	procRegistry(t, z)
	if got, ok := FindLeadTeam(z); !ok || got.Dir != tz {
		t.Fatalf("FindLeadTeam(resumed after a crash) = %+v, %v; want %s", got, ok, tz)
	}
	if got, ok := LeadForTeam("session-106024d3", []leadsession.Session{z}); ok {
		t.Fatalf("LeadForTeam(crashed lead's team) = %+v, want !ok (orphaned)", got)
	}
	// A -p resume of the crashed lead's id has no team of its own and gets
	// none; the crashed lead's team stays orphaned.
	pr := procSession(z.SessionID, "/work/p", procStartedAt+400_000)
	pr.PID, pr.Entrypoint = 400, "sdk-cli"
	procRegistry(t, pr)
	if got, ok := FindLeadTeam(pr); ok {
		t.Fatalf("FindLeadTeam(-p resume of a crashed lead's id) = %+v, want !ok", got)
	}
	if got, ok := LeadForTeam("session-106024d3", []leadsession.Session{pr}); ok {
		t.Fatalf("LeadForTeam(crashed lead's team, live=[-p resume]) = %+v, want !ok (orphaned)", got)
	}

	// Two `claude --continue` started together register the same resumed id;
	// each keeps its own team.
	c1 := procSession("5bedb4e2-0000-4000-8000-000000000001", "/work/q", procStartedAt+300_609)
	c1.PID, c1.ProcStart = 300, strconv.FormatInt(procStartedAt+300_000, 10)
	c2 := c1
	c2.PID, c2.StartedAt = 301, procStartedAt+300_640
	procRegistry(t, c1, c2)
	d1 := procWriteTeam(t, "session-2f8d3f00", "2f8d3f00-0000-4000-8000-000000000001", "/work/q", procStartedAt+300_529)
	d2 := procWriteTeam(t, "session-fc97a2d4", "fc97a2d4-0000-4000-8000-000000000001", "/work/q", procStartedAt+300_551)
	for s, want := range map[*leadsession.Session]string{&c1: d1, &c2: d2} {
		if got, ok := FindLeadTeam(*s); !ok || got.Dir != want {
			t.Fatalf("FindLeadTeam(--continue pid %d) = %+v, %v; want %s", s.PID, got, ok, want)
		}
	}
}

// TestFindLeadTeamClockStep: on Linux the process start is converted with the
// current boot time, so a forward wall-clock step after the lead started moves
// it later by the step (whole seconds: btime is whole seconds). The lead keeps
// its team whatever the step: with its own id after a long startup dialog, and
// after /clear with a short dialog or none.
func TestFindLeadTeamClockStep(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	procDrifts()
	const p0 = procStartedAt - 24_300 // process start; a 24.3s trust dialog
	for _, step := range []int64{0, 5_000, 12_000, 20_000, 60_000, 3_600_000} {
		s := procSession("b1b2c3d4-0000-4000-8000-000000000001", "/work/p", procStartedAt)
		s.ProcStart = strconv.FormatInt(p0+step, 10)
		procRegistry(t, s)
		own := procWriteTeam(t, "session-b1b2c3d4", s.SessionID, s.Cwd, p0+200)
		if got, ok := FindLeadTeam(s); !ok || got.Dir != own {
			t.Fatalf("step %dms, own id: FindLeadTeam = %+v, %v; want %s", step, got, ok, own)
		}
		if got, ok := LeadForTeam("session-b1b2c3d4", []leadsession.Session{s}); !ok || got != s {
			t.Fatalf("step %dms, own id: LeadForTeam = %+v, %v; want the lead", step, got, ok)
		}
	}
	hermeticHome(t)
	const p1 = procStartedAt - 5_000 // a 5s dialog, then /clear
	for _, step := range []int64{0, 1_000, 2_000, 4_000, 6_000, 3_600_000} {
		s := procSession("ffff0000-0000-4000-8000-000000000006", "/work/p", procStartedAt)
		s.ProcStart = strconv.FormatInt(p1+step, 10)
		procRegistry(t, s)
		own := procWriteTeam(t, "session-a1b2c3d4", "a1b2c3d4-0000-4000-8000-000000000001", s.Cwd, p1+300)
		if got, ok := FindLeadTeam(s); !ok || got.Dir != own {
			t.Fatalf("step %dms, after /clear: FindLeadTeam = %+v, %v; want %s", step, got, ok, own)
		}
	}
	// No dialog: team at +600ms, registered at +800ms; a step that crosses a
	// btime second moves the start to +700ms, between the two.
	hermeticHome(t)
	s := procSession("eeee0000-0000-4000-8000-000000000007", "/work/p", procStartedAt+800)
	s.ProcStart = strconv.FormatInt(procStartedAt+1_000, 10)
	procRegistry(t, s)
	own := procWriteTeam(t, "session-c1b2c3d4", "c1b2c3d4-0000-4000-8000-000000000001", s.Cwd, procStartedAt+600)
	leadStartMsFn = func(leadsession.Session) (int64, bool) { return procStartedAt + 700, true }
	if got, ok := FindLeadTeam(s); !ok || got.Dir != own {
		t.Fatalf("sub-second step, after /clear: FindLeadTeam = %+v, %v; want %s", got, ok, own)
	}

	// Keeping the team named after its id when its window is empty does not
	// let a -p resume of a /clear'd lead's id take that lead's team.
	hermeticHome(t)
	procFakeStart(t)
	procDrifts()
	x := procSession("84a7d10f-0000-4000-8000-000000000001", "/work/p", procStartedAt) // /clear'd, was 106024d3…
	x.PID = 100
	pr := procSession("106024d3-0000-4000-8000-000000000001", "/work/p", procStartedAt+400_000)
	pr.PID, pr.Entrypoint = 400, "sdk-cli"
	procRegistry(t, x, pr)
	t0 := procWriteTeam(t, "session-106024d3", pr.SessionID, "/work/p", procStartedAt-194)
	if got, ok := FindLeadTeam(pr); ok {
		t.Fatalf("FindLeadTeam(-p resume of a /clear'd lead's id) = %+v, want !ok", got)
	}
	if got, ok := FindLeadTeam(x); !ok || got.Dir != t0 {
		t.Fatalf("FindLeadTeam(/clear'd lead) = %+v, %v; want %s", got, ok, t0)
	}
}

// TestFindLeadTeamInSessionResume: /resume inside a running lead registers the
// resumed conversation's id without a new team. An older lead that resumed a
// newer lead's conversation keeps its own team, and the newer lead keeps its
// team whether it /clear'd, kept the id, or crashed.
func TestFindLeadTeamInSessionResume(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	l := procSession("5902055b-0000-4000-8000-000000000001", "/work/p", procStartedAt) // /resume'd m's conversation
	l.PID = 100
	m := procSession("5e58d4dc-0000-4000-8000-000000000001", "/work/p", procStartedAt+15_600) // /clear'd, was 5902055b…
	m.PID = 200
	tl := procWriteTeam(t, "session-a178ec4b", "a178ec4b-0000-4000-8000-000000000001", "/work/p", procStartedAt-150)
	tm := procWriteTeam(t, "session-5902055b", l.SessionID, "/work/p", procStartedAt+15_450)

	mKept := m
	mKept.SessionID = l.SessionID
	for _, mm := range []leadsession.Session{m, mKept} {
		procRegistry(t, l, mm)
		for s, want := range map[*leadsession.Session]string{&l: tl, &mm: tm} {
			if got, ok := FindLeadTeam(*s); !ok || got.Dir != want {
				t.Fatalf("m %s: FindLeadTeam(pid %d) = %+v, %v; want %s", mm.SessionID[:8], s.PID, got, ok, want)
			}
		}
		for _, live := range [][]leadsession.Session{{l, mm}, {mm, l}} {
			if got, ok := LeadForTeam("session-a178ec4b", live); !ok || got != l {
				t.Fatalf("m %s: LeadForTeam(l's team) = %+v, %v; want l", mm.SessionID[:8], got, ok)
			}
			if got, ok := LeadForTeam("session-5902055b", live); !ok || got != mm {
				t.Fatalf("m %s: LeadForTeam(m's team) = %+v, %v; want m", mm.SessionID[:8], got, ok)
			}
		}
	}

	// m crashed: l keeps its team, m's is orphaned.
	procRegistry(t, l)
	if got, ok := FindLeadTeam(l); !ok || got.Dir != tl {
		t.Fatalf("m crashed: FindLeadTeam(l) = %+v, %v; want %s", got, ok, tl)
	}
	if got, ok := LeadForTeam("session-5902055b", []leadsession.Session{l}); ok {
		t.Fatalf("m crashed: LeadForTeam(m's team) = %+v, want !ok (orphaned)", got)
	}
}

// TestFindLeadTeamWorktree: --worktree and EnterWorktree move the registered
// cwd to <dir>/.claude/worktrees/<name> while the team keeps the launch
// directory. Such a lead finds its team after /clear, and while it keeps its id
// no lead in the launch directory can take that team.
func TestFindLeadTeamWorktree(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	w := procSession("aaaa0000-0000-4000-8000-000000000001", "/work/p/.claude/worktrees/f", procStartedAt) // /clear'd
	w.PID = 100
	procRegistry(t, w)
	tw := procWriteTeam(t, "session-a1a1a1a1", "a1a1a1a1-0000-4000-8000-000000000001", "/work/p", procStartedAt-300)
	procWriteTeam(t, "session-a2a2a2a2", "a2a2a2a2-0000-4000-8000-000000000001", "/work/q", procStartedAt-200) // another project
	if got, ok := FindLeadTeam(w); !ok || got.Dir != tw {
		t.Fatalf("worktree lead after /clear: FindLeadTeam = %+v, %v; want %s", got, ok, tw)
	}
	if got, ok := FindLeadTeam(procSession(w.SessionID, "/work/q/.claude/worktrees/g", procStartedAt)); ok && got.Dir == tw {
		t.Fatalf("worktree of another project: FindLeadTeam = %+v, want not %s", got, tw)
	}

	// b: a 30s trust dialog in /work/p, then /clear. a started in /work/p during
	// that dialog, moved into a worktree and kept its id.
	hermeticHome(t)
	b := procSession("bbbb0000-0000-4000-8000-000000000002", "/work/p", procStartedAt)
	b.PID, b.ProcStart = 200, strconv.FormatInt(procStartedAt-30_000, 10)
	a := procSession("a1a1a1a1-0000-4000-8000-000000000001", "/work/p/.claude/worktrees/x", procStartedAt-10_000)
	a.PID, a.ProcStart = 300, strconv.FormatInt(procStartedAt-11_000, 10)
	procRegistry(t, b, a)
	tb := procWriteTeam(t, "session-b1b1b1b1", "b1b1b1b1-0000-4000-8000-000000000001", "/work/p", procStartedAt-29_700)
	ta := procWriteTeam(t, "session-a1a1a1a1", a.SessionID, "/work/p", procStartedAt-10_300)
	if got, ok := FindLeadTeam(b); !ok || got.Dir != tb {
		t.Fatalf("launch-dir lead: FindLeadTeam = %+v, %v; want %s", got, ok, tb)
	}
	if got, ok := FindLeadTeam(a); !ok || got.Dir != ta {
		t.Fatalf("worktree lead: FindLeadTeam = %+v, %v; want %s", got, ok, ta)
	}
	for team, want := range map[string]leadsession.Session{"session-b1b1b1b1": b, "session-a1a1a1a1": a} {
		if got, ok := LeadForTeam(team, []leadsession.Session{a, b}); !ok || got != want {
			t.Fatalf("LeadForTeam(%s) = %+v, %v; want pid %d", team, got, ok, want.PID)
		}
	}

	// --worktree roots the worktree at the main checkout, also from a
	// subdirectory or from inside another worktree.
	for _, launch := range []string{"/work/p/sub", "/work/p/.claude/worktrees/a"} {
		hermeticHome(t)
		w := procSession("aaaa0000-0000-4000-8000-000000000001", "/work/p/.claude/worktrees/b", procStartedAt) // /clear'd
		procRegistry(t, w)
		tw := procWriteTeam(t, "session-a1a1a1a1", "a1a1a1a1-0000-4000-8000-000000000001", launch, procStartedAt-300)
		if got, ok := FindLeadTeam(w); !ok || got.Dir != tw {
			t.Fatalf("worktree lead launched in %s: FindLeadTeam = %+v, %v; want %s", launch, got, ok, tw)
		}
	}

	// Leads started together that cannot claim by id (/clear, --continue),
	// teams created in the order the leads registered: each keeps its own
	// team, whatever directories they started in or moved to, and a teamless
	// session (-p, SDK) gets none. Times after t0: process start, team
	// creation, registration. Every layout is also run with the team files
	// read in the opposite order.
	type lead struct {
		cwd, teamCwd     string // teamCwd "": a teamless session
		start, team, reg int64
	}
	const p = "/work/p"
	const wa, wb, wv, ww = p + "/.claude/worktrees/a", p + "/.claude/worktrees/b", p + "/.claude/worktrees/v", p + "/.claude/worktrees/w"
	t0 := procStartedAt/1000*1000 - 5_000
	for _, tc := range []struct {
		name   string
		drifts bool
		leads  []lead
	}{
		{"checkout, launched in a worktree", false, []lead{{p, p, 0, 300, 900}, {wv, wv, 0, 320, 1_100}}},
		{"launched in and moved into one worktree", false, []lead{{ww, ww, 100, 300, 900}, {ww, p, 250, 450, 1_100}}},
		{"moved into and launched in one worktree", false, []lead{{ww, p, 100, 300, 900}, {ww, ww, 250, 450, 1_100}}},
		{"launched, moved, 5s apart", true, []lead{{ww, ww, 0, 200, 600}, {ww, p, 5_000, 5_200, 5_700}}},
		{"launched, checkout, moved", false, []lead{{ww, ww, 100, 300, 900}, {p, p, 150, 400, 1_000}, {ww, p, 250, 450, 1_100}}},
		{"launched, teamless in the checkout, moved", false, []lead{{ww, ww, 100, 300, 900}, {p, "", 600, 0, 1_000}, {ww, p, 250, 450, 1_100}}},
		{"crossing moves", false, []lead{{wv, p, 100, 300, 900}, {ww, wv, 250, 450, 1_100}}},
		{"crossing moves, drifting start", true, []lead{{wv, p, 100, 300, 900}, {ww, wv, 250, 450, 1_100}}},
		{"-w a, checkout, -w b", false, []lead{{wa, p, 800, 950, 1_900}, {p, p, 1_000, 1_150, 2_000}, {wb, p, 900, 1_200, 2_200}}},
		{"-w a, teamless in the checkout, -w b", false, []lead{{wa, p, 500, 700, 1_000}, {p, "", 1_050, 0, 1_800}, {wb, p, 900, 1_100, 2_200}}},
	} {
		for _, rev := range []bool{false, true} {
			hermeticHome(t)
			leadStartDrifts = tc.drifts
			var live []leadsession.Session
			teams := map[int]string{}
			for i, l := range tc.leads {
				s := procSession(fmt.Sprintf("dd%06x-0000-4000-8000-000000000000", i), l.cwd, t0+l.reg)
				s.PID, s.ProcStart = 100+i, strconv.FormatInt(t0+l.start, 10)
				live = append(live, s)
				if l.teamCwd == "" {
					continue
				}
				n := i
				if rev {
					n = len(tc.leads) - 1 - i
				}
				id := fmt.Sprintf("ab%06x-0000-4000-8000-000000000000", n)
				teams[i] = procWriteTeam(t, "session-"+id[:8], id, l.teamCwd, t0+l.team)
			}
			procRegistry(t, live...)
			for i, s := range live {
				got, ok := FindLeadTeam(s)
				if dir, has := teams[i]; ok != has || has && got.Dir != dir {
					t.Fatalf("%s (reversed %v): FindLeadTeam(lead %d) = %+v, %v; want %q", tc.name, rev, i, got, ok, dir)
				}
				if dir, has := teams[i]; has {
					if got, ok := LeadForTeam(filepath.Base(dir), live); !ok || got != s {
						t.Fatalf("%s (reversed %v): LeadForTeam(%s) = %+v, %v; want pid %d", tc.name, rev, filepath.Base(dir), got, ok, s.PID)
					}
				}
			}
		}
	}

	// Teams created in the same millisecond cannot be told apart, also across
	// the checkout and a worktree: the lead registered last fails closed.
	hermeticHome(t)
	leadStartDrifts = false
	m := procSession("cccc0000-0000-4000-8000-000000000003", p, t0+900)
	m.PID, m.ProcStart = 100, strconv.FormatInt(t0, 10)
	c := procSession("dddd0000-0000-4000-8000-000000000004", wv, t0+1_100)
	c.PID, c.ProcStart = 200, strconv.FormatInt(t0, 10)
	procRegistry(t, m, c)
	procWriteTeam(t, "session-e1e1e1e1", "e1e1e1e1-0000-4000-8000-000000000003", p, t0+320)
	procWriteTeam(t, "session-c1c1c1c1", "c1c1c1c1-0000-4000-8000-000000000004", wv, t0+320)
	if got, ok := FindLeadTeam(c); ok {
		t.Fatalf("teams in the same ms: FindLeadTeam = %+v, want !ok", got)
	}
}

// TestFindLeadTeamSameMsRegistration: two /clear'd leads that registered in
// the same millisecond cannot be told apart even when their teams were not
// created in the same one: FindLeadTeam reports no team; LeadForTeam gives
// each team one of them, the same way whatever the order of live.
func TestFindLeadTeamSameMsRegistration(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	p1 := procSession("f4a10000-0000-4000-8000-000000000001", "/work/p", procStartedAt)
	p1.PID, p1.ProcStart = 29403, strconv.FormatInt(procStartedAt-1_000, 10)
	p2 := p1
	p2.PID, p2.SessionID = 29417, "d77c0000-0000-4000-8000-000000000002"
	procRegistry(t, p1, p2)
	procWriteTeam(t, "session-f4a13cb7", "f4a13cb7-0000-4000-8000-000000000001", "/work/p", procStartedAt-70)
	procWriteTeam(t, "session-d77c6bdf", "d77c6bdf-0000-4000-8000-000000000002", "/work/p", procStartedAt-67)
	for _, s := range []leadsession.Session{p1, p2} {
		if got, ok := FindLeadTeam(s); ok {
			t.Fatalf("FindLeadTeam(pid %d) = %+v, want !ok (registered in the same ms)", s.PID, got)
		}
	}
	for _, live := range [][]leadsession.Session{{p1, p2}, {p2, p1}} {
		a, okA := LeadForTeam("session-f4a13cb7", live)
		b, okB := LeadForTeam("session-d77c6bdf", live)
		if !okA || !okB || a == b {
			t.Fatalf("LeadForTeam = %+v %v, %+v %v; want one lead each", a, okA, b, okB)
		}
	}
}

// TestFindLeadTeamRejectsStale: a long-dead session team (the session-d94a5eb3
// shape: same cwd, created weeks earlier, lead gone) never matches a new lead.
func TestFindLeadTeamRejectsStale(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	procRegistry(t)
	procWriteTeam(t, "session-d94a5eb3", "d94a5eb3-6c1e-4a8f-9b0d-2f7e5c1a9e44", "/Users/u", 1_783_900_000_000)
	// Unparseable and lead-less configs are ignored, not fatal.
	for name, body := range map[string]string{"session-0badc0de": "{not json", "session-00000000": `{"createdAt":1789999999500}`} {
		dir := filepath.Join(claudepaths.Teams(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s := procSession("5e55a0e1-0000-4000-8000-000000000003", "/Users/u", procStartedAt)
	if got, ok := FindLeadTeam(s); ok {
		t.Fatalf("FindLeadTeam = %+v, want !ok for a stale team", got)
	}
	if got, ok := FindLeadTeam(procSession("00000000-0000-4000-8000-000000000000", "/Users/u", procStartedAt)); ok {
		t.Fatalf("FindLeadTeam(lead-less config) = %+v, want !ok", got)
	}
	if got, ok := FindLeadTeam(leadsession.Session{}); ok {
		t.Fatalf("FindLeadTeam(zero session) = %+v, want !ok", got)
	}
}

func TestLeadForTeam(t *testing.T) {
	hermeticHome(t)
	procFakeStart(t)
	owner := procSession("aaaaaaaa-0000-4000-8000-000000000001", "/work/a", procStartedAt)
	owner.PID = 100
	cleared := procSession("cccc0000-0000-4000-8000-000000000002", "/work/b", procStartedAt+50_000)
	cleared.PID = 200
	procRegistry(t, owner, cleared)

	procWriteTeam(t, "session-aaaaaaaa", owner.SessionID, owner.Cwd, procStartedAt-200)
	procWriteTeam(t, "session-bbbbbbbb", "bbbbbbbb-0000-4000-8000-000000000001", cleared.Cwd, cleared.StartedAt-900)
	procWriteTeam(t, "session-d94a5eb3", "d94a5eb3-6c1e-4a8f-9b0d-2f7e5c1a9e44", "/work/a", 1_783_900_000_000)

	live := []leadsession.Session{cleared, owner}
	if got, ok := LeadForTeam("session-aaaaaaaa", live); !ok || got != owner {
		t.Fatalf("LeadForTeam(session-aaaaaaaa) = %+v, %v; want owner %+v", got, ok, owner)
	}
	if got, ok := LeadForTeam("session-bbbbbbbb", live); !ok || got != cleared {
		t.Fatalf("LeadForTeam(session-bbbbbbbb) = %+v, %v; want cleared %+v", got, ok, cleared)
	}
	for _, team := range []string{"session-d94a5eb3", "session-eeeeeeee", ""} {
		if got, ok := LeadForTeam(team, live); ok {
			t.Errorf("LeadForTeam(%q) = %+v, want !ok (orphaned)", team, got)
		}
	}
	if got, ok := LeadForTeam("session-aaaaaaaa", nil); ok {
		t.Fatalf("LeadForTeam with no live sessions = %+v, want !ok", got)
	}

	// Two fallback matches, possible only when the registry read missed them
	// (a file being rewritten): the session that registered first after the
	// team was created wins, whatever the order of live.
	near := procSession("eeee0000-0000-4000-8000-000000000004", "/work/c", procStartedAt+200_000)
	near.PID = 400
	far := procSession("ffff0000-0000-4000-8000-000000000005", "/work/c", procStartedAt+205_000)
	far.PID, far.ProcStart = 500, strconv.FormatInt(procStartedAt+199_000, 10)
	procRegistry(t)
	procWriteTeam(t, "session-cccccccc", "cccccccc-0000-4000-8000-000000000001", "/work/c", procStartedAt+199_500)
	for _, live := range [][]leadsession.Session{{far, near}, {near, far}} {
		if got, ok := LeadForTeam("session-cccccccc", live); !ok || got != near {
			t.Fatalf("LeadForTeam(two fallback matches) = %+v, %v; want the first to register %+v", got, ok, near)
		}
	}
}
