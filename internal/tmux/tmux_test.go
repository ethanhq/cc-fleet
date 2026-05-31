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

// mockTmuxRouter installs a fake `tmux` that dispatches on the tmux subcommand
// (argv[0]) so a single test can drive SplitWindow's multi-command flow:
// display-message, list-panes, and split-window each emit their own configured
// stdout, while select-layout / resize-pane (and anything else) succeed
// silently. Every invocation's argv is appended to $MOCK_ARGS_FILE exactly like
// mockTmux. Per-command stdout comes from $MOCK_DISPLAY_OUT /
// $MOCK_LISTPANES_OUT / $MOCK_SPLIT_OUT (set via setRouterOutputs);
// $MOCK_EXIT_CODE still applies to every call.
func mockTmuxRouter(t *testing.T) string {
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
  display-message) printf '%s' "$MOCK_DISPLAY_OUT" ;;
  list-panes)      printf '%s' "$MOCK_LISTPANES_OUT" ;;
  split-window)    printf '%s' "$MOCK_SPLIT_OUT" ;;
esac
exit "${MOCK_EXIT_CODE:-0}"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock tmux: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	t.Setenv("MOCK_EXIT_CODE", "0")
	t.Setenv("MOCK_DISPLAY_OUT", "")
	t.Setenv("MOCK_LISTPANES_OUT", "")
	t.Setenv("MOCK_SPLIT_OUT", "")
	return argsPath
}

// setRouterOutputs configures the per-command stdout for mockTmuxRouter.
// listPanes may be multi-line (one pane id per line) to drive the pane count.
func setRouterOutputs(t *testing.T, display, listPanes, split string) {
	t.Helper()
	t.Setenv("MOCK_DISPLAY_OUT", display)
	t.Setenv("MOCK_LISTPANES_OUT", listPanes)
	t.Setenv("MOCK_SPLIT_OUT", split)
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

// TestSplitWindow_FirstTeammate: with a single pane in the window, the teammate
// is split off the resolved leader pane at 70% (leader keeps 30%), and no
// main-vertical reflow happens — a two-pane split is already balanced.
func TestSplitWindow_FirstTeammate(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	// leader %84, window has 1 pane, new pane %42.
	setRouterOutputs(t, "%84", "%84", "%42")

	pane, err := SplitWindow("%84", "h", "echo hello", "cyan", "alice")
	if err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	if pane != "%42" {
		t.Fatalf("pane = %q, want %q", pane, "%42")
	}

	calls := readMockArgs(t, argsPath)
	split := findCall(t, calls, "split-window")
	want := []string{
		"split-window",
		"-t", "%84",
		"-h",
		"-l", "70%",
		"-d",
		"-P",
		"-F", "#{pane_id}",
		"echo hello",
	}
	if !reflect.DeepEqual(split, want) {
		t.Fatalf("split argv mismatch:\n got: %v\nwant: %v", split, want)
	}
	// A balanced two-pane split needs no reflow.
	if hasCall(calls, "select-layout") || hasCall(calls, "resize-pane") {
		t.Fatalf("unexpected reflow for first teammate; calls = %v", calls)
	}
}

// TestSplitWindow_FirstTeammate_Vertical: direction "v" is honored for the
// first teammate (new pane below the leader), and 70% sizing still applies.
func TestSplitWindow_FirstTeammate_Vertical(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	setRouterOutputs(t, "%0", "%0", "%9")

	if _, err := SplitWindow("mysess:2", "v", "ls", "cyan", "bob"); err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	split := findCall(t, readMockArgs(t, argsPath), "split-window")
	if split[3] != "-v" {
		t.Fatalf("direction flag = %q, want -v", split[3])
	}
	if !reflect.DeepEqual(split[4:6], []string{"-l", "70%"}) {
		t.Fatalf("size flags = %v, want [-l 70%%]", split[4:6])
	}
}

// TestSplitWindow_AdditionalTeammate_Rebalances: with 2+ panes already in the
// window, the teammate is split off the leader vertically (preserving leader
// width), then the window is reflowed to main-vertical and the resolved leader
// pane — not list-panes[0] — is pinned to 30% width.
func TestSplitWindow_AdditionalTeammate_Rebalances(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	// leader %84, window already has 2 panes, new teammate pane %99.
	setRouterOutputs(t, "%84", "%84\n%85", "%99")

	pane, err := SplitWindow("%84", "h", "run me", "magenta", "carol")
	if err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	if pane != "%99" {
		t.Fatalf("pane = %q, want %q", pane, "%99")
	}

	calls := readMockArgs(t, argsPath)

	// Split is vertical off the leader, with no 70% sizing (reflow handles it).
	split := findCall(t, calls, "split-window")
	wantSplit := []string{
		"split-window", "-t", "%84", "-v", "-d", "-P", "-F", "#{pane_id}", "run me",
	}
	if !reflect.DeepEqual(split, wantSplit) {
		t.Fatalf("split argv mismatch:\n got: %v\nwant: %v", split, wantSplit)
	}

	// Reflow: main-vertical, then the resolved leader pinned to 30%.
	layout := findCall(t, calls, "select-layout")
	wantLayout := []string{"select-layout", "-t", "%84", "main-vertical"}
	if !reflect.DeepEqual(layout, wantLayout) {
		t.Fatalf("select-layout argv mismatch:\n got: %v\nwant: %v", layout, wantLayout)
	}
	resize := findCall(t, calls, "resize-pane")
	wantResize := []string{"resize-pane", "-t", "%84", "-x", "30%"}
	if !reflect.DeepEqual(resize, wantResize) {
		t.Fatalf("resize-pane argv mismatch:\n got: %v\nwant: %v", resize, wantResize)
	}
}

// TestSplitWindow_FallbackWhenIntrospectionFails: when tmux can't resolve the
// leader pane (display-message yields nothing), SplitWindow falls back to a
// plain split off the original target honoring direction — no 70% sizing and
// no reflow.
func TestSplitWindow_FallbackWhenIntrospectionFails(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	// display-message returns empty → introspection fails; only the split works.
	setRouterOutputs(t, "", "", "%7")

	pane, err := SplitWindow("mysess", "h", "cmd", "yellow", "dave")
	if err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	if pane != "%7" {
		t.Fatalf("pane = %q, want %q", pane, "%7")
	}

	calls := readMockArgs(t, argsPath)
	split := findCall(t, calls, "split-window")
	want := []string{
		"split-window", "-t", "mysess", "-h", "-d", "-P", "-F", "#{pane_id}", "cmd",
	}
	if !reflect.DeepEqual(split, want) {
		t.Fatalf("fallback split argv mismatch:\n got: %v\nwant: %v", split, want)
	}
	if hasCall(calls, "select-layout") || hasCall(calls, "resize-pane") {
		t.Fatalf("fallback must not reflow; calls = %v", calls)
	}
}

func TestSplitWindow_RejectsEmptyTarget(t *testing.T) {
	mockTmux(t)
	if _, err := SplitWindow("", "h", "ls", "cyan", "x"); err == nil {
		t.Fatal("SplitWindow(\"\"): want error, got nil")
	}
}

func TestSplitWindow_RejectsBadDirection(t *testing.T) {
	mockTmux(t)
	if _, err := SplitWindow("1", "x", "ls", "cyan", "x"); err == nil {
		t.Fatal("SplitWindow direction=x: want error, got nil")
	}
}

func TestSplitWindow_RejectsEmptyCmd(t *testing.T) {
	mockTmux(t)
	if _, err := SplitWindow("1", "h", "", "cyan", "x"); err == nil {
		t.Fatal("SplitWindow cmd=\"\": want error, got nil")
	}
}

func TestSplitWindow_FailsOnTmuxError(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := SplitWindow("1", "h", "ls", "cyan", "x"); err == nil {
		t.Fatal("SplitWindow: want error when tmux exits 1, got nil")
	}
}

func TestSplitWindow_FailsOnEmptyOutput(t *testing.T) {
	mockTmux(t)
	// No MOCK_OUTPUT_FILE set → mock prints nothing.
	if _, err := SplitWindow("1", "h", "ls", "cyan", "x"); err == nil {
		t.Fatal("SplitWindow: want error when tmux prints no pane id, got nil")
	}
}

// TestSplitWindow_DecoratesPane verifies the border/title decoration: after
// the load-bearing split, the new pane gets a colored border (select-pane -P +
// per-pane border styles) and the window's pane-border-status enabled, and the
// teammate name is embedded LITERALLY in pane-border-format — NOT via
// #{pane_title}, which claude's TUI overwrites with its agent type at startup.
// The '#'-escape sub-case checks names are escaped (# -> ##) so they can't
// inject tmux format directives. The teammate color is mapped to a tmux color
// via tmuxColorName before it reaches the border (see TestTmuxColorName); "cyan"
// is identity-mapped, so the asserted fg=cyan is unaffected.
func TestSplitWindow_DecoratesPane(t *testing.T) {
	cases := []struct {
		desc       string
		agentName  string
		wantFormat string
	}{
		{"plain name", "worker", "#[fg=cyan,bold] worker #[default]"},
		{"name with hash escaped", "a#b", "#[fg=cyan,bold] a##b #[default]"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			argsPath := mockTmuxRouter(t)
			// First-teammate path: leader %84, window has 1 pane, new pane %42.
			setRouterOutputs(t, "%84", "%84", "%42")

			pane, err := SplitWindow("%84", "h", "echo hi", "cyan", tc.agentName)
			if err != nil {
				t.Fatalf("SplitWindow: %v", err)
			}
			calls := readMockArgs(t, argsPath)

			// Colored border + border-status (e2e-verified, must be issued).
			for _, want := range [][]string{
				{"select-pane", "-t", pane, "-P", "bg=default,fg=cyan"},
				{"set-option", "-p", "-t", pane, "pane-border-style", "fg=cyan"},
				{"set-option", "-p", "-t", pane, "pane-active-border-style", "fg=cyan"},
				{"set-option", "-w", "-t", pane, "pane-border-status", "top"},
			} {
				if !hasCallArgs(calls, want) {
					t.Errorf("missing decoration call:\n want: %v\n all:  %v", want, calls)
				}
			}
			// Name embedded literally in the border-format.
			wantFmt := []string{"set-option", "-p", "-t", pane, "pane-border-format", tc.wantFormat}
			if !hasCallArgs(calls, wantFmt) {
				t.Errorf("missing/incorrect pane-border-format:\n want: %v\n all:  %v", wantFmt, calls)
			}
			// The border-format must NOT reference the overwritten #{pane_title}.
			for _, c := range calls {
				if len(c) >= 6 && c[0] == "set-option" && c[4] == "pane-border-format" &&
					strings.Contains(c[5], "#{pane_title}") {
					t.Errorf("pane-border-format still uses #{pane_title} (claude overwrites it): %v", c)
				}
			}
			// And `select-pane -T` is gone (it only set the clobbered title).
			for _, c := range calls {
				if len(c) >= 4 && c[0] == "select-pane" && c[3] == "-T" {
					t.Errorf("select-pane -T should no longer be issued; got %v", c)
				}
			}
		})
	}
}

// TestTmuxColorName checks the AgentColorName -> tmux color mapping: the three
// names tmux doesn't understand (purple/orange/pink) are translated to their
// tmux equivalents, while tmux-native names and arbitrary explicit colors pass
// through unchanged.
func TestTmuxColorName(t *testing.T) {
	cases := map[string]string{
		"purple":    "magenta",
		"orange":    "colour208",
		"pink":      "colour205",
		"red":       "red",
		"blue":      "blue",
		"green":     "green",
		"yellow":    "yellow",
		"cyan":      "cyan",
		"magenta":   "magenta",   // explicit raw tmux name passes through
		"colour123": "colour123", // explicit 256-color passes through
		"":          "",
	}
	for in, want := range cases {
		if got := tmuxColorName(in); got != want {
			t.Errorf("tmuxColorName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitWindow_MapsAgentColorToTmux verifies decoratePane runs the teammate
// color through tmuxColorName: spawning with the AgentColorName "purple" must
// paint the border with tmux "magenta" — the literal "purple" (which tmux would
// reject) must never reach a tmux arg.
func TestSplitWindow_MapsAgentColorToTmux(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	// First-teammate path: leader %84, window has 1 pane, new pane %42.
	setRouterOutputs(t, "%84", "%84", "%42")

	pane, err := SplitWindow("%84", "h", "echo hi", "purple", "worker")
	if err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	calls := readMockArgs(t, argsPath)

	for _, want := range [][]string{
		{"select-pane", "-t", pane, "-P", "bg=default,fg=magenta"},
		{"set-option", "-p", "-t", pane, "pane-border-style", "fg=magenta"},
		{"set-option", "-p", "-t", pane, "pane-active-border-style", "fg=magenta"},
		{"set-option", "-p", "-t", pane, "pane-border-format", "#[fg=magenta,bold] worker #[default]"},
	} {
		if !hasCallArgs(calls, want) {
			t.Errorf("missing mapped-color call:\n want: %v\n all:  %v", want, calls)
		}
	}
	// The unmapped AgentColorName must not leak into any tmux arg.
	for _, c := range calls {
		for _, a := range c {
			if strings.Contains(a, "purple") {
				t.Errorf("tmux arg leaked unmapped color \"purple\": %v", c)
			}
		}
	}
}

func TestPickAttachedSession_PicksWidest(t *testing.T) {
	mockTmux(t)
	setMockOutput(t, "1 1 200\n0 1 300\nbg 0 500\n")
	got, err := PickAttachedSession()
	if err != nil {
		t.Fatalf("PickAttachedSession: %v", err)
	}
	if got != "0" {
		t.Fatalf("picked %q, want %q (widest attached)", got, "0")
	}
}

func TestPickAttachedSession_NoneAttached(t *testing.T) {
	mockTmux(t)
	setMockOutput(t, "bg 0 500\n")
	if _, err := PickAttachedSession(); err == nil {
		t.Fatal("PickAttachedSession: want error when none attached, got nil")
	}
}

func TestPickAttachedSession_TmuxError(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := PickAttachedSession(); err == nil {
		t.Fatal("PickAttachedSession: want error when tmux fails, got nil")
	}
}

// TestPickAttachedSession_MultiClientAttached guards the attached-count parse:
// session_attached is a client count, so a session with 2+ clients ("2") must
// still count as attached. (Regression: code required exactly "1".)
func TestPickAttachedSession_MultiClientAttached(t *testing.T) {
	mockTmux(t)
	setMockOutput(t, "main 2 271\n")
	got, err := PickAttachedSession()
	if err != nil {
		t.Fatalf("PickAttachedSession: %v", err)
	}
	if got != "main" {
		t.Fatalf("picked %q, want %q (2 clients still attached)", got, "main")
	}
}

func TestSessionForPane_ReturnsName(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "cc-fleet\n")
	got, err := SessionForPane("%84")
	if err != nil {
		t.Fatalf("SessionForPane: %v", err)
	}
	if got != "cc-fleet" {
		t.Fatalf("got %q, want %q", got, "cc-fleet")
	}
	calls := readMockArgs(t, argsPath)
	want := []string{"display-message", "-p", "-t", "%84", "#{session_name}"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", calls[0], want)
	}
}

func TestSessionForPane_RejectsEmpty(t *testing.T) {
	mockTmux(t)
	if _, err := SessionForPane(""); err == nil {
		t.Fatal("SessionForPane(\"\"): want error, got nil")
	}
}

func TestSessionForPane_TmuxError(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := SessionForPane("%1"); err == nil {
		t.Fatal("SessionForPane: want error when tmux fails, got nil")
	}
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

// hasSub reports whether any recorded invocation's subcommand — argv after an
// optional leading "-L <socket>" prefix — equals sub. findCall/hasCall key on
// argv[0], which is "-L" for socket-scoped calls, so socket tests use this.
func hasSub(calls [][]string, sub string) bool {
	for _, c := range calls {
		i := 0
		if len(c) >= 2 && c[0] == "-L" {
			i = 2
		}
		if len(c) > i && c[i] == sub {
			return true
		}
	}
	return false
}

// TestServerCommand_ArgvScoping is the load-bearing regression for the socket
// abstraction: the default Server (socket=="") must produce argv byte-identical
// to a bare exec.Command — NO stray empty -L, which tmux rejects — while a named
// socket inserts exactly "-L <socket>" before the subcommand. Every in-tmux call
// goes through socket=="", so this guards the zero-regression invariant.
func TestServerCommand_ArgvScoping(t *testing.T) {
	got := Server{}.command("list-panes", "-a", "-F", "x").Args
	want := exec.Command(tmuxBinary, "list-panes", "-a", "-F", "x").Args
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default-socket argv drift:\n got: %v\nwant: %v", got, want)
	}
	gotL := NewServer("cc-fleet-swarm-t").command("kill-server").Args
	wantL := []string{tmuxBinary, "-L", "cc-fleet-swarm-t", "kill-server"}
	if !reflect.DeepEqual(gotL, wantL) {
		t.Fatalf("named-socket argv:\n got: %v\nwant: %v", gotL, wantL)
	}
}

// TestSplitWindow_RebalanceResizesMainPaneNotLeader: when the resolved leader
// pane is NOT list-panes[0], the rebalance resize must target list[0] (the pane
// select-layout main-vertical promotes to main), NOT the leader — keeping spawn
// and hide→show unified. (When leader==list[0] the two coincide — see
// TestSplitWindow_AdditionalTeammate_Rebalances.)
func TestSplitWindow_RebalanceResizesMainPaneNotLeader(t *testing.T) {
	argsPath := mockTmuxRouter(t)
	// leader=%84, but the window lists %55 first (list[0]) then %84 → leader is
	// not list[0]. paneCount=2 (>1) → rebalance path; new pane %99.
	setRouterOutputs(t, "%84", "%55\n%84", "%99")
	if _, err := SplitWindow("%84", "h", "run", "cyan", "carol"); err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}
	resize := findCall(t, readMockArgs(t, argsPath), "resize-pane")
	want := []string{"resize-pane", "-t", "%55", "-x", "30%"}
	if !reflect.DeepEqual(resize, want) {
		t.Fatalf("rebalance must resize list[0]=%%55 (MV main pane), not leader %%84:\n got: %v\nwant: %v", resize, want)
	}
}

// TestListPanes_SocketScoped: the Server-scoped ListPanes prefixes -L <socket>
// and still parses, so ps/teardown can enumerate an out-of-tmux swarm server.
func TestListPanes_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "%1 claude-swarm 0 1 0 claude\n")
	panes, err := NewServer("cc-fleet-swarm-t").ListPanes()
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 1 || panes[0].SessionName != "claude-swarm" {
		t.Fatalf("unexpected panes: %+v", panes)
	}
	want := []string{"-L", "cc-fleet-swarm-t", "list-panes", "-a", "-F", listPanesFormat}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L list-panes call; got %v", readMockArgs(t, argsPath))
	}
}

// TestKillPane_SocketScoped: KillPane on a socket prefixes -L so teardown can
// kill panes on the swarm server (default-server kill-pane would "can't find" and
// be swallowed — the silent-leak hazard).
func TestKillPane_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	if err := NewServer("sock").KillPane("%9"); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	want := []string{"-L", "sock", "kill-pane", "-t", "%9"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L kill-pane call; got %v", readMockArgs(t, argsPath))
	}
}

// TestKillServer_RefusesDefault: KillServer must never kill the default tmux
// server (the in-tmux user's own); only a named socket is allowed.
func TestKillServer_RefusesDefault(t *testing.T) {
	mockTmux(t)
	if err := (Server{}).KillServer(); err == nil {
		t.Fatal("KillServer on default server: want error, got nil")
	}
}

// TestKillServer_SocketAndIdempotent: KillServer on a socket emits
// `-L <socket> kill-server` and treats "no server running" as success.
func TestKillServer_SocketAndIdempotent(t *testing.T) {
	argsPath := mockTmux(t)
	if err := NewServer("cc-fleet-swarm-t").KillServer(); err != nil {
		t.Fatalf("KillServer: %v", err)
	}
	want := []string{"-L", "cc-fleet-swarm-t", "kill-server"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L kill-server call; got %v", readMockArgs(t, argsPath))
	}
	// "no server running" exit 1 → idempotent success.
	t.Setenv("MOCK_EXIT_CODE", "1")
	setMockOutput(t, "no server running on /tmp/tmux-0/cc-fleet-swarm-t\n")
	if err := NewServer("cc-fleet-swarm-t").KillServer(); err != nil {
		t.Fatalf("KillServer should swallow no-server: %v", err)
	}
}

// mockTmuxSwarm drives SpawnSwarm: has-session exits MOCK_HASSESSION_EXIT
// (default 0 = exists); new-session and split-window print MOCK_PANE_OUT; all
// else succeeds silently. Records full argv (incl. any -L prefix) like mockTmux,
// shifting past a leading "-L <socket>" only to DISPATCH on the subcommand.
func mockTmuxSwarm(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"; done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
if [ "$1" = "-L" ]; then shift 2; fi
case "$1" in
  has-session)  exit "${MOCK_HASSESSION_EXIT:-0}" ;;
  new-session)  printf '%s' "$MOCK_PANE_OUT" ;;
  split-window) printf '%s' "$MOCK_PANE_OUT" ;;
esac
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	t.Setenv("MOCK_HASSESSION_EXIT", "0")
	t.Setenv("MOCK_PANE_OUT", "")
	return argsPath
}

// TestSpawnSwarm_FirstTeammateCreatesSession: with no existing swarm session,
// SpawnSwarm creates the detached claude-swarm session + swarm-view window on the
// socket and does NOT split or tile (the initial pane is the first teammate).
func TestSpawnSwarm_FirstTeammateCreatesSession(t *testing.T) {
	argsPath := mockTmuxSwarm(t)
	t.Setenv("MOCK_HASSESSION_EXIT", "1") // session absent
	t.Setenv("MOCK_PANE_OUT", "%5")
	pane, createdServer, err := NewServer("cc-fleet-swarm-t").SpawnSwarm("echo hi", "cyan", "alice")
	if err != nil {
		t.Fatalf("SpawnSwarm: %v", err)
	}
	if pane != "%5" {
		t.Fatalf("pane = %q, want %%5", pane)
	}
	// Creating the session ⇒ first member ⇒ createdServer true.
	if !createdServer {
		t.Fatalf("createdServer = false, want true (first teammate created the session)")
	}
	calls := readMockArgs(t, argsPath)
	want := []string{"-L", "cc-fleet-swarm-t", "new-session", "-d", "-s", "claude-swarm",
		"-n", "swarm-view", "-P", "-F", "#{pane_id}", "echo hi"}
	if !hasCallArgs(calls, want) {
		t.Fatalf("missing new-session call:\n want: %v\n all:  %v", want, calls)
	}
	if hasSub(calls, "split-window") {
		t.Fatalf("first teammate must not split-window; calls = %v", calls)
	}
}

// TestSpawnSwarm_AdditionalTeammateTiles: with an existing swarm session,
// SpawnSwarm splits a new pane into swarm-view and tiles (no leader).
func TestSpawnSwarm_AdditionalTeammateTiles(t *testing.T) {
	argsPath := mockTmuxSwarm(t)
	t.Setenv("MOCK_HASSESSION_EXIT", "0") // session exists
	t.Setenv("MOCK_PANE_OUT", "%6")
	pane, createdServer, err := NewServer("cc-fleet-swarm-t").SpawnSwarm("echo hi", "blue", "bob")
	if err != nil {
		t.Fatalf("SpawnSwarm: %v", err)
	}
	if pane != "%6" {
		t.Fatalf("pane = %q, want %%6", pane)
	}
	// Session already exists ⇒ later member ⇒ createdServer false, so a failed
	// spawn of this member must NOT kill the whole server.
	if createdServer {
		t.Fatalf("createdServer = true, want false (additional teammate joined an existing session)")
	}
	calls := readMockArgs(t, argsPath)
	wantSplit := []string{"-L", "cc-fleet-swarm-t", "split-window", "-t", "claude-swarm:swarm-view",
		"-d", "-P", "-F", "#{pane_id}", "echo hi"}
	if !hasCallArgs(calls, wantSplit) {
		t.Fatalf("missing split-window call:\n want: %v\n all:  %v", wantSplit, calls)
	}
	wantTiled := []string{"-L", "cc-fleet-swarm-t", "select-layout", "-t", "claude-swarm:swarm-view", "tiled"}
	if !hasCallArgs(calls, wantTiled) {
		t.Fatalf("missing tiled reflow:\n want: %v\n all:  %v", wantTiled, calls)
	}
	if hasSub(calls, "new-session") {
		t.Fatalf("additional teammate must not new-session; calls = %v", calls)
	}
}

// TestSpawnSwarm_RejectsDefaultSocket: SpawnSwarm requires a private server.
func TestSpawnSwarm_RejectsDefaultSocket(t *testing.T) {
	mockTmuxSwarm(t)
	if _, _, err := (Server{}).SpawnSwarm("cmd", "cyan", "x"); err == nil {
		t.Fatal("SpawnSwarm on default socket: want error, got nil")
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

// TestPickAttachedSession_SocketScoped: PickAttachedSession on a named socket
// must prefix `-L <socket>` so a caller that runs against a private server picks
// an attached session there, not on the default server. The default-server form
// (Server{}) is the in-tmux path and stays byte-stable.
func TestPickAttachedSession_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main 1 200\n")
	name, err := NewServer("cc-fleet-swarm-t").PickAttachedSession()
	if err != nil {
		t.Fatalf("PickAttachedSession: %v", err)
	}
	if name != "main" {
		t.Fatalf("name = %q, want main", name)
	}
	want := []string{"-L", "cc-fleet-swarm-t", "ls", "-F", listSessionsFormat}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L ls call; got %v", readMockArgs(t, argsPath))
	}
}

// TestPickAttachedSession_DefaultArgvStable: Server{} (socket=="") must keep
// the bare exec.Command argv form — no stray empty -L, no drift — so every
// in-tmux call site behaves byte-identically.
func TestPickAttachedSession_DefaultArgvStable(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main 1 200\n")
	if _, err := PickAttachedSession(); err != nil {
		t.Fatalf("PickAttachedSession: %v", err)
	}
	want := []string{"ls", "-F", listSessionsFormat}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("default-server PickAttachedSession should emit bare argv; got %v",
			readMockArgs(t, argsPath))
	}
}

// TestSessionForPane_SocketScoped: SessionForPane on a named socket must prefix
// `-L <socket>` so the display-message hits the right server.
func TestSessionForPane_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main\n")
	name, err := NewServer("cc-fleet-swarm-t").SessionForPane("%84")
	if err != nil {
		t.Fatalf("SessionForPane: %v", err)
	}
	if name != "main" {
		t.Fatalf("name = %q, want main", name)
	}
	want := []string{"-L", "cc-fleet-swarm-t", "display-message", "-p", "-t", "%84", "#{session_name}"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L display-message call; got %v", readMockArgs(t, argsPath))
	}
}

// TestSessionForPane_DefaultArgvStable: Server{} (socket=="") must keep the
// bare exec.Command argv form so the in-tmux path stays byte-identical.
func TestSessionForPane_DefaultArgvStable(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main\n")
	if _, err := SessionForPane("%84"); err != nil {
		t.Fatalf("SessionForPane: %v", err)
	}
	want := []string{"display-message", "-p", "-t", "%84", "#{session_name}"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("default-server SessionForPane should emit bare argv; got %v",
			readMockArgs(t, argsPath))
	}
}

// TestListPanesWithPid_SocketScoped: routing the discover pipeline through
// Server.ListPanesWithPid funnels every tmux exec through the one outlet AND
// still socket-scopes for swarm panes.
func TestListPanesWithPid_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "%5 1234\n%6 5678\n")
	pairs, err := NewServer("cc-fleet-swarm-t").ListPanesWithPid()
	if err != nil {
		t.Fatalf("ListPanesWithPid: %v", err)
	}
	if len(pairs) != 2 || pairs[0].PaneID != "%5" || pairs[0].PID != 1234 ||
		pairs[1].PaneID != "%6" || pairs[1].PID != 5678 {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}
	want := []string{"-L", "cc-fleet-swarm-t", "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L list-panes call; got %v", readMockArgs(t, argsPath))
	}
}

// TestListPanesWithPid_DefaultArgvStable: socket=="" → bare argv. In-tmux
// discover.go calls listPanesWithPid("") for the default server, so this is
// the load-bearing default path.
func TestListPanesWithPid_DefaultArgvStable(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "%5 1234\n")
	if _, err := (Server{}).ListPanesWithPid(); err != nil {
		t.Fatalf("ListPanesWithPid: %v", err)
	}
	want := []string{"list-panes", "-a", "-F", "#{pane_id} #{pane_pid}"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("default-server ListPanesWithPid should emit bare argv; got %v",
			readMockArgs(t, argsPath))
	}
}

// TestListPanesWithPid_NoServerEmpty: tmux exit-1 with no server running must
// surface as (nil, nil) so `ps` works even with tmux down.
func TestListPanesWithPid_NoServerEmpty(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	pairs, err := (Server{}).ListPanesWithPid()
	if err != nil {
		t.Fatalf("ListPanesWithPid no-server: %v", err)
	}
	if pairs != nil {
		t.Fatalf("expected nil pairs, got %+v", pairs)
	}
}

// TestCapturePane_Server_SocketScoped: the pane-scan path
// (teardown.AnnotateHealth → capturePane) is funneled through Server.CapturePane
// so the single outlet invariant holds AND swarm panes still scope to their
// private server.
func TestCapturePane_Server_SocketScoped(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "pane contents\n")
	got, err := NewServer("cc-fleet-swarm-t").CapturePane("%9")
	if err != nil {
		t.Fatalf("CapturePane: %v", err)
	}
	if got != "pane contents\n" {
		t.Fatalf("CapturePane = %q, want %q", got, "pane contents\n")
	}
	want := []string{"-L", "cc-fleet-swarm-t", "capture-pane", "-t", "%9", "-p"}
	if !hasCallArgs(readMockArgs(t, argsPath), want) {
		t.Fatalf("missing -L capture-pane call; got %v", readMockArgs(t, argsPath))
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
