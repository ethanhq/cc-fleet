package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mockTmux installs a fake `tmux` shell script into a fresh temp dir and
// prepends that dir to PATH. The script writes its argv (one per line) to
// $MOCK_ARGS_FILE, then prints the contents of $MOCK_OUTPUT_FILE to stdout
// and exits with $MOCK_EXIT_CODE (default 0). Tests then set those env
// vars per-invocation to script behavior.
//
// Returns the path to the args-log file so tests can assert the args we
// passed.
func mockTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")

	// The script intentionally writes args.log via O_APPEND so multiple calls
	// in one test accumulate. Tests that care about the latest call should
	// clear the file before invoking the code under test, or just look at the
	// last N lines.
	script := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"
done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
if [ -n "$MOCK_OUTPUT_FILE" ] && [ -f "$MOCK_OUTPUT_FILE" ]; then
  cat "$MOCK_OUTPUT_FILE"
fi
exit "${MOCK_EXIT_CODE:-0}"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock tmux: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	t.Setenv("MOCK_OUTPUT_FILE", "")
	t.Setenv("MOCK_EXIT_CODE", "0")

	// The package-level tmuxBinary var is "tmux" — exec.LookPath will use
	// the test-modified PATH and find ours first.
	return argsPath
}

// setMockOutput writes lines to a file and points MOCK_OUTPUT_FILE at it.
func setMockOutput(t *testing.T, lines string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdout.txt")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatalf("write mock output: %v", err)
	}
	t.Setenv("MOCK_OUTPUT_FILE", p)
}

// readMockArgs returns the recorded invocations as [][]string. Each inner
// slice is one invocation's argv (without the binary itself).
func readMockArgs(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mock args: %v", err)
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

// findCall returns the first recorded invocation whose argv[0] == sub, failing
// the test if there is none.
func findCall(t *testing.T, calls [][]string, sub string) []string {
	t.Helper()
	for _, c := range calls {
		if len(c) > 0 && c[0] == sub {
			return c
		}
	}
	t.Fatalf("no %q call recorded; calls = %v", sub, calls)
	return nil
}

// hasCall reports whether any recorded invocation has argv[0] == sub.
func hasCall(calls [][]string, sub string) bool {
	for _, c := range calls {
		if len(c) > 0 && c[0] == sub {
			return true
		}
	}
	return false
}

// hasCallArgs reports whether any recorded invocation's argv exactly equals want.
func hasCallArgs(calls [][]string, want []string) bool {
	for _, c := range calls {
		if reflect.DeepEqual(c, want) {
			return true
		}
	}
	return false
}

func TestListPanes_Parses(t *testing.T) {
	mockTmux(t)
	setMockOutput(t,
		"%1 1 0 1 1 claude\n"+
			"%2 1 0 0 1 zsh\n"+
			"%3 bg 2 1 0 vim\n",
	)
	panes, err := ListPanes()
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	want := []Pane{
		{PaneID: "%1", SessionName: "1", WindowIndex: 0, PaneActive: true, Attached: true, Command: "claude"},
		{PaneID: "%2", SessionName: "1", WindowIndex: 0, PaneActive: false, Attached: true, Command: "zsh"},
		{PaneID: "%3", SessionName: "bg", WindowIndex: 2, PaneActive: true, Attached: false, Command: "vim"},
	}
	if !reflect.DeepEqual(panes, want) {
		t.Fatalf("ListPanes mismatch:\n got: %+v\nwant: %+v", panes, want)
	}
}

func TestListPanes_EmptyServer(t *testing.T) {
	mockTmux(t)
	setMockOutput(t, "")
	panes, err := ListPanes()
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 0 {
		t.Fatalf("expected empty slice, got %+v", panes)
	}
}

func TestListPanes_TmuxError(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := ListPanes(); err == nil {
		t.Fatal("ListPanes: want error when tmux fails, got nil")
	}
}

func TestParseListPanes_BadLine(t *testing.T) {
	if _, err := parseListPanes("only three fields\n"); err == nil {
		t.Fatal("parseListPanes: want error on malformed line, got nil")
	}
}

// TestParseListPanes_MultiClientAttached guards the attached-count parse:
// session_attached is a client count, so "2" (two attached clients) must
// report Attached=true. (Regression: code used `== "1"`.)
func TestParseListPanes_MultiClientAttached(t *testing.T) {
	// fields: pane_id session window_index pane_active session_attached command
	panes, err := parseListPanes("%7 main 0 1 2 claude\n")
	if err != nil {
		t.Fatalf("parseListPanes: %v", err)
	}
	if len(panes) != 1 {
		t.Fatalf("got %d panes, want 1", len(panes))
	}
	if !panes[0].Attached {
		t.Fatal("Attached = false, want true (session_attached=2 means 2 clients)")
	}
}

func TestKillPane_OK(t *testing.T) {
	argsPath := mockTmux(t)
	if err := KillPane("%42"); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	calls := readMockArgs(t, argsPath)
	want := []string{"kill-pane", "-t", "%42"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", calls[0], want)
	}
}

func TestKillPane_RejectsEmpty(t *testing.T) {
	mockTmux(t)
	if err := KillPane(""); err == nil {
		t.Fatal("KillPane(\"\"): want error, got nil")
	}
}

func TestKillPane_IdempotentNoSuch(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	setMockOutput(t, "can't find pane: %99\n")
	if err := KillPane("%99"); err != nil {
		t.Fatalf("KillPane should swallow can't-find: got %v", err)
	}
}

func TestKillPane_FailsOnRealError(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	setMockOutput(t, "server not running\n")
	if err := KillPane("%1"); err == nil {
		t.Fatal("KillPane: want error on real failure, got nil")
	}
}

// mockTmuxPerCmd installs a fake `tmux` that exits per-subcommand so a single
// test can make (say) break-pane fail while new-session succeeds. Each command's
// exit code comes from MOCK_<CMD>_EXIT (default 0); display-message / list-panes
// also print MOCK_DISPLAY_OUT / MOCK_LISTPANES_OUT. Every invocation's argv is
// appended to MOCK_ARGS_FILE exactly like mockTmux.
func mockTmuxPerCmd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"
done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
case "$1" in
  new-session)     exit "${MOCK_NEWSESSION_EXIT:-0}" ;;
  break-pane)      exit "${MOCK_BREAKPANE_EXIT:-0}" ;;
  join-pane)       exit "${MOCK_JOINPANE_EXIT:-0}" ;;
  select-layout)   exit "${MOCK_SELECTLAYOUT_EXIT:-0}" ;;
  list-panes)      printf '%s' "$MOCK_LISTPANES_OUT"; exit "${MOCK_LISTPANES_EXIT:-0}" ;;
  resize-pane)     exit 0 ;;
  display-message) printf '%s' "$MOCK_DISPLAY_OUT"; exit "${MOCK_DISPLAY_EXIT:-0}" ;;
esac
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	return argsPath
}

// TestHidePane_EmitsSequence: HidePane creates the hidden session then breaks
// the pane into it, in that order, with the expected argv.
func TestHidePane_EmitsSequence(t *testing.T) {
	argsPath := mockTmuxPerCmd(t)
	if err := HidePane("%42"); err != nil {
		t.Fatalf("HidePane: %v", err)
	}
	calls := readMockArgs(t, argsPath)
	wantNew := []string{"new-session", "-d", "-s", "claude-hidden"}
	if !reflect.DeepEqual(findCall(t, calls, "new-session"), wantNew) {
		t.Fatalf("new-session argv = %v, want %v", findCall(t, calls, "new-session"), wantNew)
	}
	wantBreak := []string{"break-pane", "-d", "-s", "%42", "-t", "claude-hidden:"}
	if !reflect.DeepEqual(findCall(t, calls, "break-pane"), wantBreak) {
		t.Fatalf("break-pane argv = %v, want %v", findCall(t, calls, "break-pane"), wantBreak)
	}
	// Order: new-session must precede break-pane.
	var iNew, iBreak = -1, -1
	for i, c := range calls {
		if len(c) > 0 && c[0] == "new-session" {
			iNew = i
		}
		if len(c) > 0 && c[0] == "break-pane" {
			iBreak = i
		}
	}
	if iNew < 0 || iBreak < 0 || iNew > iBreak {
		t.Fatalf("expected new-session before break-pane; new=%d break=%d", iNew, iBreak)
	}
}

// TestHidePane_NewSessionErrSwallowed: a failing new-session (the duplicate-
// session idempotent case) must not fail HidePane when break-pane succeeds.
func TestHidePane_NewSessionErrSwallowed(t *testing.T) {
	mockTmuxPerCmd(t)
	t.Setenv("MOCK_NEWSESSION_EXIT", "1")
	if err := HidePane("%42"); err != nil {
		t.Fatalf("HidePane should swallow new-session error: %v", err)
	}
}

// TestHidePane_BreakPaneFails: the load-bearing break-pane failing returns err.
func TestHidePane_BreakPaneFails(t *testing.T) {
	mockTmuxPerCmd(t)
	t.Setenv("MOCK_BREAKPANE_EXIT", "1")
	if err := HidePane("%42"); err == nil {
		t.Fatal("HidePane: want error when break-pane fails, got nil")
	}
}

func TestHidePane_RejectsEmpty(t *testing.T) {
	mockTmuxPerCmd(t)
	if err := HidePane(""); err == nil {
		t.Fatal("HidePane(\"\"): want error, got nil")
	}
}

// TestShowPane_EmitsSequence: ShowPane joins the pane back, reflows main-vertical,
// then resizes list-panes[0] (the leader) to 30%.
func TestShowPane_EmitsSequence(t *testing.T) {
	argsPath := mockTmuxPerCmd(t)
	t.Setenv("MOCK_LISTPANES_OUT", "%7\n%42\n%43\n")
	if err := ShowPane("%42", "main:0"); err != nil {
		t.Fatalf("ShowPane: %v", err)
	}
	calls := readMockArgs(t, argsPath)
	wantJoin := []string{"join-pane", "-h", "-s", "%42", "-t", "main:0"}
	if !reflect.DeepEqual(findCall(t, calls, "join-pane"), wantJoin) {
		t.Fatalf("join-pane argv = %v, want %v", findCall(t, calls, "join-pane"), wantJoin)
	}
	wantLayout := []string{"select-layout", "-t", "main:0", "main-vertical"}
	if !reflect.DeepEqual(findCall(t, calls, "select-layout"), wantLayout) {
		t.Fatalf("select-layout argv = %v, want %v", findCall(t, calls, "select-layout"), wantLayout)
	}
	wantList := []string{"list-panes", "-t", "main:0", "-F", "#{pane_id}"}
	if !reflect.DeepEqual(findCall(t, calls, "list-panes"), wantList) {
		t.Fatalf("list-panes argv = %v, want %v", findCall(t, calls, "list-panes"), wantList)
	}
	// Leader is list-panes[0] = %7 → resized to 30%.
	wantResize := []string{"resize-pane", "-t", "%7", "-x", "30%"}
	if !reflect.DeepEqual(findCall(t, calls, "resize-pane"), wantResize) {
		t.Fatalf("resize-pane argv = %v, want %v", findCall(t, calls, "resize-pane"), wantResize)
	}
}

// TestShowPane_JoinFails: a failing join-pane (load-bearing) returns err and no
// layout polish is attempted.
func TestShowPane_JoinFails(t *testing.T) {
	argsPath := mockTmuxPerCmd(t)
	t.Setenv("MOCK_JOINPANE_EXIT", "1")
	if err := ShowPane("%42", "main:0"); err == nil {
		t.Fatal("ShowPane: want error when join-pane fails, got nil")
	}
	calls := readMockArgs(t, argsPath)
	if hasCall(calls, "select-layout") || hasCall(calls, "resize-pane") {
		t.Fatalf("join-pane failure must skip layout polish; calls = %v", calls)
	}
}

// TestShowPane_SelectLayoutFailsIgnored: select-layout failing is best-effort —
// ShowPane still returns nil and still attempts the resize.
func TestShowPane_SelectLayoutFailsIgnored(t *testing.T) {
	argsPath := mockTmuxPerCmd(t)
	t.Setenv("MOCK_SELECTLAYOUT_EXIT", "1")
	t.Setenv("MOCK_LISTPANES_OUT", "%7\n%42\n")
	if err := ShowPane("%42", "main:0"); err != nil {
		t.Fatalf("ShowPane should ignore select-layout failure: %v", err)
	}
	if !hasCall(readMockArgs(t, argsPath), "resize-pane") {
		t.Fatal("resize-pane should still be attempted after a select-layout failure")
	}
}

func TestShowPane_RejectsEmpty(t *testing.T) {
	mockTmuxPerCmd(t)
	if err := ShowPane("", "main:0"); err == nil {
		t.Fatal("ShowPane(empty pane): want error, got nil")
	}
	if err := ShowPane("%1", ""); err == nil {
		t.Fatal("ShowPane(empty origin): want error, got nil")
	}
}

func TestDisplayMessage_ReturnsTrimmed(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main:0\n")
	got, err := DisplayMessage("%42", "#{session_name}:#{window_index}")
	if err != nil {
		t.Fatalf("DisplayMessage: %v", err)
	}
	if got != "main:0" {
		t.Fatalf("DisplayMessage = %q, want %q", got, "main:0")
	}
	want := []string{"display-message", "-p", "-t", "%42", "#{session_name}:#{window_index}"}
	if !reflect.DeepEqual(readMockArgs(t, argsPath)[0], want) {
		t.Fatalf("argv = %v, want %v", readMockArgs(t, argsPath)[0], want)
	}
}

func TestDisplayMessage_Errors(t *testing.T) {
	mockTmux(t)
	if _, err := DisplayMessage("", "fmt"); err == nil {
		t.Fatal("DisplayMessage(empty target): want error, got nil")
	}
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := DisplayMessage("%1", "fmt"); err == nil {
		t.Fatal("DisplayMessage: want error when tmux fails, got nil")
	}
}

// TestServerCommand_ArgvScoping is the load-bearing regression for the socket
// abstraction: the default Server must produce argv byte-identical to a bare
// exec.Command, while a socket path inserts exactly "-S <path>" before the
// subcommand.
func TestServerCommand_ArgvScoping(t *testing.T) {
	got := Server{}.command("list-panes", "-a", "-F", "x").Args
	want := exec.Command(tmuxBinary, "list-panes", "-a", "-F", "x").Args
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default-socket argv drift:\n got: %v\nwant: %v", got, want)
	}
	gotS := NewServerPath("/tmp/tmux-501/claude-swarm-1").command("kill-pane").Args
	wantS := []string{tmuxBinary, "-S", "/tmp/tmux-501/claude-swarm-1", "kill-pane"}
	if !reflect.DeepEqual(gotS, wantS) {
		t.Fatalf("socket-path argv:\n got: %v\nwant: %v", gotS, wantS)
	}
}

// TestListPanes_SocketScoped: the Server-scoped ListPanes prefixes -S <path>
// and still parses.
func TestListPanes_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "%1 claude-swarm 0 1 0 claude\n")
	panes, err := NewServerPath("/tmp/tmux-501/claude-swarm-1").ListPanes()
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 || panes[0].SessionName != "claude-swarm" {
		t.Fatalf("unexpected panes: %+v", panes)
	}
	want := []string{"-S", "/tmp/tmux-501/claude-swarm-1", "list-panes", "-a", "-F", listPanesFormat}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -S list-panes call; got %v", readMockArgs(t, argsPath))
	}
}

// TestKillPane_SocketScoped: KillPane on a socket path prefixes -S so teardown
// can kill panes on another server (default-server kill-pane would "can't find" and
// be swallowed — the silent-leak hazard).
func TestKillPane_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	if err := NewServerPath("/tmp/tmux-501/sock").KillPane("%9"); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	want := []string{"-S", "/tmp/tmux-501/sock", "kill-pane", "-t", "%9"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -S kill-pane call; got %v", readMockArgs(t, argsPath))
	}
}

func TestQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"plain", "'plain'"},
		{"spaces are ok", "'spaces are ok'"},
		{"$dangerous;rm -rf /", "'$dangerous;rm -rf /'"},
		{"it's", `'it'\''s'`},
		{"a'b'c", `'a'\''b'\''c'`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := Quote(tc.in); got != tc.want {
				t.Fatalf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCapturePane_Server_SocketScoped: the pane-scan path
// (teardown.AnnotateHealth → capturePane) is funneled through Server.CapturePane
// so the single outlet invariant holds AND panes on another server still scope
// to it.
func TestCapturePane_Server_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "pane contents\n")
	got, err := NewServerPath("/tmp/tmux-501/claude-swarm-1").CapturePane("%9")
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if got != "pane contents\n" {
		t.Fatalf("CapturePane = %q, want %q", got, "pane contents\n")
	}
	want := []string{"-S", "/tmp/tmux-501/claude-swarm-1", "capture-pane", "-t", "%9", "-p"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -S capture-pane call; got %v", readMockArgs(t, argsPath))
	}
}

// TestCapturePane_Server_RejectsEmpty: empty pane id is a programmer error,
// rejected explicitly so we never `capture-pane -t ` (which would attach to
// the current pane).
func TestCapturePane_Server_RejectsEmpty(t *testing.T) {
	mockTmux(t)
	if _, err := (Server{}).CapturePane(""); err == nil {
		t.Fatal("CapturePane(\"\"): want error, got nil")
	}
}
