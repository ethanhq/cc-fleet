package panevis

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// fakeTmux installs a per-subcommand fake `tmux` on PATH (same scheme as
// internal/tmux tests). display-message prints MOCK_DISPLAY_OUT; list-panes
// prints MOCK_LISTPANES_OUT; break-pane/join-pane/display-message honor
// MOCK_*_EXIT so a test can fail exactly one op. Records argv to MOCK_ARGS_FILE.
func fakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"; done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
# Socket-scoped (swarm) calls are "-L <socket> <subcommand> ...". Skip a leading
# -L so dispatch keys on the subcommand; the full argv (incl. -L) is already
# recorded above so a test can assert the socket flag was passed.
if [ "$1" = "-L" ]; then shift 2; fi
case "$1" in
  new-session)     exit "${MOCK_NEWSESSION_EXIT:-0}" ;;
  break-pane)      exit "${MOCK_BREAKPANE_EXIT:-0}" ;;
  join-pane)       exit "${MOCK_JOINPANE_EXIT:-0}" ;;
  select-layout)   exit 0 ;;
  list-panes)      printf '%s' "$MOCK_LISTPANES_OUT"; exit 0 ;;
  resize-pane)     exit 0 ;;
  display-message) printf '%s' "$MOCK_DISPLAY_OUT"; exit "${MOCK_DISPLAY_EXIT:-0}" ;;
esac
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	return argsPath
}

// setup points HOME at a temp dir (for team config + flock) and installs the
// fake tmux. Returns the recorded-args path.
func setup(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return fakeTmux(t)
}

// mkMember builds a member with the fields hide/show care about.
func mkMember(name, pane string, hidden bool, origin string) spawn.Member {
	return spawn.Member{
		AgentID: name + "@team", Name: name, AgentType: "general-purpose",
		Model: "glm-4.6", JoinedAt: 1, TmuxPaneID: pane, Cwd: "/x",
		Subscriptions: []string{}, BackendType: "tmux", IsActive: true,
		Hidden: hidden, OriginWindow: origin,
	}
}

func seedTeam(t *testing.T, team string, members []spawn.Member) {
	t.Helper()
	if err := spawn.EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	tc := &spawn.TeamConfig{LeadSessionID: "lead", Members: members, Raw: map[string]any{}}
	if err := spawn.WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
}

func recordedCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil // no tmux invocations recorded
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "__END__" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		if line == "" {
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

func hasSub(calls [][]string, sub string) bool {
	for _, c := range calls {
		if len(c) > 0 && c[0] == sub {
			return true
		}
	}
	return false
}

func TestResolve_AllForms(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{
		mkMember("alice", "%84", false, ""),
		mkMember("bob", "%85", false, ""),
		mkMember("lead", "", false, ""), // no pane → excluded from bare-team expansion
	})

	// Resolve carries the effective socket (empty here — default server, no
	// swarm socket seeded) + the config pane id for each target.
	if got, err := Resolve("%85"); err != nil || len(got) != 1 || got[0] != (Target{Team: "alpha", Name: "bob", PaneID: "%85"}) {
		t.Fatalf("%%pane: got=%v err=%v", got, err)
	}
	if got, err := Resolve("alpha/alice"); err != nil || len(got) != 1 || got[0] != (Target{Team: "alpha", Name: "alice", PaneID: "%84"}) {
		t.Fatalf("team/member: got=%v err=%v", got, err)
	}
	if got, err := Resolve("alice@alpha"); err != nil || len(got) != 1 || got[0] != (Target{Team: "alpha", Name: "alice", PaneID: "%84"}) {
		t.Fatalf("name@team: got=%v err=%v", got, err)
	}

	got, err := Resolve("alpha") // bare team → only members with a pane
	if err != nil || len(got) != 2 {
		t.Fatalf("bare team: got=%v err=%v, want 2", got, err)
	}
	names := map[string]bool{}
	for _, g := range got {
		names[g.Name] = true
	}
	if !names["alice"] || !names["bob"] || names["lead"] {
		t.Fatalf("bare team expansion = %v, want alice+bob (no lead)", got)
	}

	if _, err := Resolve(""); err == nil {
		t.Error("empty target should error")
	}
	if _, err := Resolve("%999"); err == nil {
		t.Error("unknown pane should error")
	}
	if _, err := Resolve("no-such-team"); err == nil {
		t.Error("missing team should error")
	}
}

// TestResolve_ErrorCodes pins the ResolveError codes so a skill switching on
// error_code sees TEAM_NOT_FOUND / PANE_NOT_FOUND, not a blanket BAD_ARGS
// (reviewer S1). The command layer (cmd/cc-fleet/hide.go) extracts re.Code.
func TestResolve_ErrorCodes(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})

	cases := []struct {
		target string
		want   string
	}{
		{"", ErrBadArgs},                  // empty
		{"alpha/", ErrBadArgs},            // malformed team/member
		{"@alpha", ErrBadArgs},            // malformed name@team
		{"%999", ErrPaneNotFound},         // unknown pane
		{"no-such-team", ErrTeamNotFound}, // missing bare team
	}
	for _, c := range cases {
		_, err := Resolve(c.target)
		if err == nil {
			t.Errorf("Resolve(%q): want error %s, got nil", c.target, c.want)
			continue
		}
		var re *ResolveError
		if !errors.As(err, &re) {
			t.Errorf("Resolve(%q): error is not *ResolveError: %v", c.target, err)
			continue
		}
		if re.Code != c.want {
			t.Errorf("Resolve(%q): code = %s, want %s", c.target, re.Code, c.want)
		}
	}
}

func TestHide_HappyPath(t *testing.T) {
	argsPath := setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0")
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})

	res := Hide("alpha", "alice")
	if !res.OK || !res.Hidden || res.PaneID != "%84" || res.Action != "hide" {
		t.Fatalf("Hide: %+v", res)
	}
	tc, err := spawn.LoadTeamConfig("alpha")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !tc.Members[0].Hidden || tc.Members[0].OriginWindow != "main:0" {
		t.Fatalf("config not updated on hide: %+v", tc.Members[0])
	}
	if !hasSub(recordedCalls(t, argsPath), "break-pane") {
		t.Fatal("expected a break-pane call")
	}
}

func TestHide_Idempotent(t *testing.T) {
	argsPath := setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0")
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", true, "main:0")})

	res := Hide("alpha", "alice")
	if !res.OK || !res.Hidden {
		t.Fatalf("idempotent hide should be OK+hidden: %+v", res)
	}
	if hasSub(recordedCalls(t, argsPath), "break-pane") {
		t.Fatal("idempotent hide must not re-break an already-hidden pane")
	}
}

func TestHide_PaneNotFound(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "", false, "")}) // no pane id
	res := Hide("alpha", "alice")
	if res.OK || res.ErrorCode != ErrPaneNotFound {
		t.Fatalf("Hide no-pane: %+v, want PANE_NOT_FOUND", res)
	}
}

func TestHide_PaneGone(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_EXIT", "1") // display-message fails → pane gone
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})
	res := Hide("alpha", "alice")
	if res.OK || res.ErrorCode != ErrPaneNotFound {
		t.Fatalf("Hide pane-gone: %+v, want PANE_NOT_FOUND", res)
	}
}

func TestHide_MemberNotFound(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})
	res := Hide("alpha", "ghost")
	if res.OK || res.ErrorCode != ErrMemberNotFound {
		t.Fatalf("Hide ghost: %+v, want MEMBER_NOT_FOUND", res)
	}
}

func TestHide_TeamNotFound(t *testing.T) {
	setup(t)
	res := Hide("nope", "x")
	if res.OK || res.ErrorCode != ErrTeamNotFound {
		t.Fatalf("Hide missing team: %+v, want TEAM_NOT_FOUND", res)
	}
}

func TestHide_TmuxFailedDoesNotWriteConfig(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0")
	t.Setenv("MOCK_BREAKPANE_EXIT", "1") // break-pane fails
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})

	res := Hide("alpha", "alice")
	if res.OK || res.ErrorCode != ErrTmuxFailed {
		t.Fatalf("Hide tmux-fail: %+v, want TMUX_FAILED", res)
	}
	tc, _ := spawn.LoadTeamConfig("alpha")
	if tc.Members[0].Hidden {
		t.Fatal("config must NOT record hidden when break-pane failed")
	}
}

func TestShow_HappyPath(t *testing.T) {
	argsPath := setup(t)
	t.Setenv("MOCK_LISTPANES_OUT", "%7\n%84\n")
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", true, "main:0")})

	res := Show("alpha", "alice")
	if !res.OK || res.Hidden || res.Action != "show" {
		t.Fatalf("Show: %+v", res)
	}
	// Reloading proves the Raw-shadow defeat: if the keys weren't deleted from
	// Raw, the stale hidden:true would shadow through and reload as Hidden=true.
	tc, err := spawn.LoadTeamConfig("alpha")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if tc.Members[0].Hidden || tc.Members[0].OriginWindow != "" {
		t.Fatalf("config not cleared on show: %+v", tc.Members[0])
	}
	if !hasSub(recordedCalls(t, argsPath), "join-pane") {
		t.Fatal("expected a join-pane call")
	}
}

func TestShow_NotHidden(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", false, "")})
	res := Show("alpha", "alice")
	if res.OK || res.ErrorCode != ErrNotHidden {
		t.Fatalf("Show not-hidden: %+v, want NOT_HIDDEN", res)
	}
}

func TestShow_NoOrigin(t *testing.T) {
	setup(t)
	seedTeam(t, "alpha", []spawn.Member{mkMember("alice", "%84", true, "")}) // hidden but no origin
	res := Show("alpha", "alice")
	if res.OK || res.ErrorCode != ErrNoOrigin {
		t.Fatalf("Show no-origin: %+v, want NO_ORIGIN", res)
	}
}

func TestBadArgs(t *testing.T) {
	setup(t)
	if res := Hide("", "x"); res.OK || res.ErrorCode != ErrBadArgs {
		t.Fatalf("Hide empty team: %+v", res)
	}
	if res := Show("alpha", ""); res.OK || res.ErrorCode != ErrBadArgs {
		t.Fatalf("Show empty name: %+v", res)
	}
}

// (Swarm-socket refusal is covered comprehensively by TestHideShowRef_SwarmRefused
// in panevis_ref_test.go, which uses the realistic seedSwarmTeam fixture.)

// TestHideRef_NotGatedForEmptySocket: an empty socket is an in-tmux teammate, so
// the swarm gate must NOT fire — hide proceeds to the normal happy path. This
// pins the gate's keying strictly to a non-empty (swarm) socket.
func TestHideRef_NotGatedForEmptySocket(t *testing.T) {
	setup(t)
	t.Setenv("MOCK_DISPLAY_OUT", "main:0")
	seedTeam(t, "inteam", []spawn.Member{mkMember("w1", "%0", false, "")})
	r := HideRef("inteam", "w1", "", "%0")
	if r.ErrorCode == ErrSwarmUnsupported {
		t.Fatalf("empty-socket hide was gated as swarm; want normal in-tmux flow")
	}
	if !r.OK || !r.Hidden {
		t.Fatalf("in-tmux hide should succeed: %+v", r)
	}
}
