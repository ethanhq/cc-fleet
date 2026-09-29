//go:build !windows

package panevis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/teardown"
)

const (
	killSock  = "/tmp/tmux-501/default"
	killAgent = "worker@session-7c8f769b"
)

// killEnv is a hide/show test environment: discovery rows, the identity
// re-verification answer, and the fake tmux's call log and origin state file.
type killEnv struct {
	t        *testing.T
	rows     []teardown.Teammate
	verify   bool
	verified int
	argsLog  string
	state    string
}

// killSetup points HOME and the config dirs at a temp dir, installs a fake tmux
// that keeps the @ccf_origin pane option in a state file, and stubs discovery
// and identity re-verification.
func killSetup(t *testing.T, rows ...teardown.Teammate) *killEnv {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))

	e := &killEnv{t: t, rows: rows, verify: true,
		argsLog: filepath.Join(root, "args.log"), state: filepath.Join(root, "origin")}
	script := `#!/bin/sh
echo "$*" >> "$KILL_ARGS"
while [ "$1" = -S ] || [ "$1" = -L ]; do shift 2; done
if [ -f "$KILL_SESS" ]; then win=@9; sess=claude-hidden; else win=@7; sess=main; fi
case "$1" in
  display-message)
    eval "fmt=\${$#}"
    case "$fmt" in
      *session_name*) echo "$win:$sess" ;;
      *) echo "$win" ;;
    esac ;;
  set-option)
    case " $* " in
      *" -u "*) rm -f "$KILL_STATE" ;;
      *) eval "val=\${$#}"; printf '%s' "$val" > "$KILL_STATE" ;;
    esac ;;
  show-options)
    if [ -f "$KILL_STATE" ]; then cat "$KILL_STATE"; echo; else echo "invalid option: @ccf_origin" >&2; exit 1; fi ;;
  break-pane) [ -z "$KILL_FAIL_BREAK" ] || exit 1; : > "$KILL_SESS" ;;
  join-pane) [ -z "$KILL_FAIL_JOIN" ] || exit 1; rm -f "$KILL_SESS" ;;
  list-panes) echo "%1" ;;
esac
exit 0
`
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KILL_ARGS", e.argsLog)
	t.Setenv("KILL_STATE", e.state)
	// The pane sits in claude-hidden while this file exists.
	sess := filepath.Join(root, "hidden")
	t.Setenv("KILL_SESS", sess)
	if len(rows) > 0 && rows[0].Hidden {
		if err := os.WriteFile(sess, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KILL_FAIL_BREAK", "")
	t.Setenv("KILL_FAIL_JOIN", "")

	origDiscover, origVerify := discoverFn, verifyFn
	t.Cleanup(func() { discoverFn, verifyFn = origDiscover, origVerify })
	discoverFn = func() ([]teardown.Teammate, error) { return e.rows, nil }
	verifyFn = func(teardown.Teammate) bool { e.verified++; return e.verify }
	return e
}

func killRow(pane string, hidden bool) teardown.Teammate {
	return teardown.Teammate{
		AgentID: killAgent, Name: "worker", Team: "session-7c8f769b", PaneID: pane, PID: 601,
		Socket: killSock, Backend: teardown.BackendTmux, State: teardown.StateRunning, Hidden: hidden,
	}
}

func (e *killEnv) calls() []string {
	data, err := os.ReadFile(e.argsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func (e *killEnv) origin() string {
	data, err := os.ReadFile(e.state)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return string(data)
}

func (e *killEnv) mutated() bool {
	for _, c := range e.calls() {
		for _, verb := range []string{"set-option", "break-pane", "join-pane"} {
			if strings.Contains(c, " "+verb+" ") {
				return true
			}
		}
	}
	return false
}

// TestHideStoresOrigin: hide records the pane's window id in @ccf_origin
// before breaking it into the hidden session, all on the pane's -S server.
func TestHideStoresOrigin(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	res := Hide("%5", "")
	if !res.OK || !res.Hidden || res.AgentID != killAgent || res.Socket != killSock || res.PaneID != "%5" {
		t.Fatalf("result = %+v", res)
	}
	if e.origin() != "@7" {
		t.Fatalf("@ccf_origin = %q, want @7", e.origin())
	}
	want := []string{
		"-S " + killSock + " display-message -p -t %5 #{window_id}:#{session_name}",
		"-S " + killSock + " show-options -p -v -t %5 @ccf_origin",
		"-S " + killSock + " set-option -p -t %5 @ccf_origin @7",
		"-S " + killSock + " new-session -d -s claude-hidden",
		"-S " + killSock + " break-pane -d -s %5 -t claude-hidden:",
	}
	if got := e.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tmux calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestHideBreakFailureUnsetsOrigin: a failed break-pane leaves no origin
// record behind and reports TMUX_FAILED.
func TestHideBreakFailureUnsetsOrigin(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	t.Setenv("KILL_FAIL_BREAK", "1")
	res := Hide(killAgent, "")
	if res.OK || res.ErrorCode != ErrTmuxFailed || res.Hidden {
		t.Fatalf("result = %+v", res)
	}
	if e.origin() != "" {
		t.Fatalf("@ccf_origin left behind: %q", e.origin())
	}
}

// TestShowRestoresAndClears: show joins the pane back into the recorded
// window and clears the record.
func TestShowRestoresAndClears(t *testing.T) {
	e := killSetup(t, killRow("%5", true))
	if err := os.WriteFile(e.state, []byte("@7"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Show("%5", killSock)
	if !res.OK || res.Hidden {
		t.Fatalf("result = %+v", res)
	}
	if e.origin() != "" {
		t.Fatalf("@ccf_origin not cleared: %q", e.origin())
	}
	calls := strings.Join(e.calls(), "\n")
	for _, want := range []string{
		"-S " + killSock + " join-pane -d -h -s %5 -t @7",
		"-S " + killSock + " set-option -p -u -t %5 @ccf_origin",
	} {
		if !strings.Contains(calls, want) {
			t.Fatalf("missing %q in:\n%s", want, calls)
		}
	}
}

// TestShowJoinFailure: a gone origin window reports TMUX_FAILED with the
// manual join-pane command, and keeps the record.
func TestShowJoinFailure(t *testing.T) {
	e := killSetup(t, killRow("%5", true))
	if err := os.WriteFile(e.state, []byte("@7"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILL_FAIL_JOIN", "1")
	res := Show("%5", "")
	if res.OK || res.ErrorCode != ErrTmuxFailed || !strings.Contains(res.Suggestion, "tmux -S "+killSock+" join-pane -s %5") {
		t.Fatalf("result = %+v", res)
	}
	if e.origin() != "@7" {
		t.Fatalf("@ccf_origin = %q, want kept", e.origin())
	}
}

// TestHideIdempotent: hiding a pane already in claude-hidden is ok and
// touches nothing.
func TestHideIdempotent(t *testing.T) {
	e := killSetup(t, killRow("%5", true))
	res := Hide("%5", "")
	if !res.OK || !res.Hidden || res.ErrorCode != "" {
		t.Fatalf("result = %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
}

// TestShowNotHidden: showing a visible pane is NOT_HIDDEN.
func TestShowNotHidden(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	res := Show(killAgent, "")
	if res.OK || res.ErrorCode != ErrNotHidden {
		t.Fatalf("result = %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
}

// TestShowNoOrigin: a hidden pane without @ccf_origin is NO_ORIGIN.
func TestShowNoOrigin(t *testing.T) {
	e := killSetup(t, killRow("%5", true))
	res := Show("%5", "")
	if res.OK || res.ErrorCode != ErrNoOrigin || res.Suggestion == "" {
		t.Fatalf("result = %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
}

// TestRefuseSwarm: teammates on claude-swarm-* / cc-fleet-swarm-* servers are
// SWARM_UNSUPPORTED for both hide and show, with no tmux call.
func TestRefuseSwarm(t *testing.T) {
	for _, sock := range []string{"/tmp/tmux-501/claude-swarm-123", "/tmp/tmux-501/cc-fleet-swarm-alpha"} {
		row := killRow("%5", false)
		row.Socket = sock
		e := killSetup(t, row)
		for _, res := range []Result{Hide("%5", ""), HideTeammate(row), ShowTeammate(row)} {
			if res.OK || res.ErrorCode != ErrSwarmUnsupported || !strings.Contains(res.Suggestion, sock) {
				t.Fatalf("%s: result = %+v", sock, res)
			}
		}
		if len(e.calls()) != 0 || e.verified != 0 {
			t.Fatalf("%s: acted: calls %v verified %d", sock, e.calls(), e.verified)
		}
	}
}

// TestRefuseNonTmuxBackend: unknown (iTerm2) and in-process teammates are
// BACKEND_UNSUPPORTED.
func TestRefuseNonTmuxBackend(t *testing.T) {
	for _, backend := range []string{teardown.BackendUnknown, teardown.BackendInProcess} {
		row := teardown.Teammate{AgentID: killAgent, Name: "worker", Team: "session-7c8f769b", PID: 601, Backend: backend}
		e := killSetup(t, row)
		for _, res := range []Result{Hide(killAgent, ""), Show(killAgent, "")} {
			if res.OK || res.ErrorCode != ErrBackendUnsupported || !strings.Contains(res.ErrorMsg, backend) {
				t.Fatalf("%s: result = %+v", backend, res)
			}
		}
		if len(e.calls()) != 0 {
			t.Fatalf("%s: tmux called: %v", backend, e.calls())
		}
	}
}

// TestLegacyTargetBadArgs: the 0.3.x bare team and team/member forms, and
// malformed ids, are BAD_ARGS with the new syntax as suggestion.
func TestLegacyTargetBadArgs(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	for _, target := range []string{"session-7c8f769b", "alpha", "alpha/worker", "", "%x", "w 1@alpha"} {
		for _, res := range []Result{Hide(target, ""), Show(target, "")} {
			if res.OK || res.ErrorCode != ErrBadArgs || !strings.Contains(res.Suggestion, "name@team") {
				t.Fatalf("%q: result = %+v", target, res)
			}
		}
	}
	if len(e.calls()) != 0 {
		t.Fatalf("tmux called: %v", e.calls())
	}
}

// TestHideShowIdentityMismatch: a row that fails re-verification is never
// mutated.
func TestHideShowIdentityMismatch(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	e.verify = false
	if res := Hide("%5", ""); res.OK || res.ErrorCode != ErrIdentityMismatch {
		t.Fatalf("hide: %+v", res)
	}
	e.rows = []teardown.Teammate{killRow("%5", true)}
	if res := Show("%5", ""); res.OK || res.ErrorCode != ErrIdentityMismatch {
		t.Fatalf("show: %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
}

// TestHideShowTargetSelection: unknown targets are PANE_NOT_FOUND; the same
// pane id on two servers is AMBIGUOUS_TARGET until --socket picks one.
func TestHideShowTargetSelection(t *testing.T) {
	other := killRow("%5", false)
	other.AgentID, other.Name, other.Socket = "w2@session-7c8f769b", "w2", "/tmp/tmux-501/work"
	e := killSetup(t, killRow("%5", false), other)

	if res := Hide("%9", ""); res.OK || res.ErrorCode != ErrPaneNotFound {
		t.Fatalf("unknown pane: %+v", res)
	}
	if res := Hide("nobody@session-7c8f769b", ""); res.OK || res.ErrorCode != ErrPaneNotFound {
		t.Fatalf("unknown agent: %+v", res)
	}
	if res := Hide("%5", ""); res.OK || res.ErrorCode != ErrAmbiguousTarget {
		t.Fatalf("ambiguous: %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
	if res := Hide("%5", "/tmp/tmux-501/work"); !res.OK || res.AgentID != "w2@session-7c8f769b" {
		t.Fatalf("with --socket: %+v", res)
	}
}

// TestFx2pvStaleHideKeepsOrigin: a second hide holding a stale visible
// snapshot (a double `h` in the TUI) re-reads the pane's session under the
// lock, keeps the origin window, and show still returns the pane there.
func TestFx2pvStaleHideKeepsOrigin(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	if res := Hide("%5", ""); !res.OK || !res.Hidden {
		t.Fatalf("first hide: %+v", res)
	}
	if res := HideTeammate(killRow("%5", false)); !res.OK || !res.Hidden || res.ErrorCode != "" {
		t.Fatalf("stale hide: %+v", res)
	}
	if e.origin() != "@7" {
		t.Fatalf("@ccf_origin = %q, want @7", e.origin())
	}
	e.rows = []teardown.Teammate{killRow("%5", true)}
	if res := Show("%5", ""); !res.OK || res.Hidden {
		t.Fatalf("show: %+v", res)
	}
	if calls := strings.Join(e.calls(), "\n"); !strings.Contains(calls, "-S "+killSock+" join-pane -d -h -s %5 -t @7") {
		t.Fatalf("show did not rejoin @7:\n%s", calls)
	}
}

// TestFx2pvStaleShowNotHidden: show holding a stale hidden snapshot of a pane
// that is visible again is NOT_HIDDEN and does not act on a leftover origin.
func TestFx2pvStaleShowNotHidden(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	if err := os.WriteFile(e.state, []byte("@3"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := ShowTeammate(killRow("%5", true))
	if res.OK || res.ErrorCode != ErrNotHidden || res.Hidden {
		t.Fatalf("result = %+v", res)
	}
	if e.mutated() {
		t.Fatalf("tmux mutated: %v", e.calls())
	}
}

// TestFx2pvHideRollbackKeepsPriorOrigin: a failed break-pane rolls back only
// the origin this hide wrote; a record that was already there is restored.
func TestFx2pvHideRollbackKeepsPriorOrigin(t *testing.T) {
	e := killSetup(t, killRow("%5", false))
	if err := os.WriteFile(e.state, []byte("@3"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KILL_FAIL_BREAK", "1")
	if res := Hide("%5", ""); res.OK || res.ErrorCode != ErrTmuxFailed || res.Hidden {
		t.Fatalf("result = %+v", res)
	}
	if e.origin() != "@3" {
		t.Fatalf("@ccf_origin = %q, want @3 kept", e.origin())
	}
}
