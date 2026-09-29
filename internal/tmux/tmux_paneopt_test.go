//go:build !windows

package tmux

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// killStatefulTmux installs a fake tmux that keeps one pane option in a file,
// answering show-options for an unset option the way tmux 3.6 does ("invalid
// option", exit 1). Every argv is logged one call per line.
func killStatefulTmux(t *testing.T) (argsLog string) {
	t.Helper()
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args.log")
	script := `#!/bin/sh
echo "$*" >> "$KILL_ARGS"
while [ "$1" = -S ] || [ "$1" = -L ]; do shift 2; done
state="$KILL_STATE"
case "$1" in
  set-option)
    case " $* " in
      *" -u "*) rm -f "$state" ;;
      *) eval "val=\${$#}"; printf '%s' "$val" > "$state" ;;
    esac ;;
  show-options)
    if [ -f "$state" ]; then cat "$state"; echo; else echo "invalid option: @ccf_origin" >&2; exit 1; fi ;;
  *) echo "unexpected $1" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("KILL_ARGS", argsLog)
	t.Setenv("KILL_STATE", filepath.Join(dir, "state"))
	return argsLog
}

// TestPaneOptionRoundTrip: set, read, unset and read again through a -S
// server; an unset option reads as "" without error.
func TestPaneOptionRoundTrip(t *testing.T) {
	argsLog := killStatefulTmux(t)
	s := NewServerPath("/tmp/tmux-501/default")

	if v, err := s.PaneOption("%3", "@ccf_origin"); err != nil || v != "" {
		t.Fatalf("unset option = %q, %v; want empty, nil", v, err)
	}
	if err := s.SetPaneOption("%3", "@ccf_origin", "@7"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.PaneOption("%3", "@ccf_origin"); err != nil || v != "@7" {
		t.Fatalf("option = %q, %v; want @7", v, err)
	}
	if err := s.UnsetPaneOption("%3", "@ccf_origin"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.PaneOption("%3", "@ccf_origin"); err != nil || v != "" {
		t.Fatalf("after unset = %q, %v", v, err)
	}

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	want := []string{
		"-S /tmp/tmux-501/default show-options -p -v -t %3 @ccf_origin",
		"-S /tmp/tmux-501/default set-option -p -t %3 @ccf_origin @7",
		"-S /tmp/tmux-501/default show-options -p -v -t %3 @ccf_origin",
		"-S /tmp/tmux-501/default set-option -p -u -t %3 @ccf_origin",
		"-S /tmp/tmux-501/default show-options -p -v -t %3 @ccf_origin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tmux calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestPaneOptionErrors: a failure other than an unset option (a gone pane)
// is an error from every pane option call.
func TestPaneOptionErrors(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	s := NewServerPath("/tmp/tmux-501/default")
	if _, err := s.PaneOption("%9", "@ccf_origin"); err == nil {
		t.Fatal("PaneOption on a failing tmux: err = nil")
	}
	if err := s.SetPaneOption("%9", "@ccf_origin", "@1"); err == nil {
		t.Fatal("SetPaneOption on a failing tmux: err = nil")
	}
	if err := s.UnsetPaneOption("%9", "@ccf_origin"); err == nil {
		t.Fatal("UnsetPaneOption on a failing tmux: err = nil")
	}
}

// TestHideShowPanePathServer: HidePane and ShowPane on a path server scope
// every command with -S <path>.
func TestHideShowPanePathServer(t *testing.T) {
	argsPath := mockTmux(t)
	s := NewServerPath("/tmp/tmux-501/work")
	if err := s.HidePane("%4"); err != nil {
		t.Fatal(err)
	}
	if err := s.ShowPane("%4", "@2"); err != nil {
		t.Fatal(err)
	}
	calls := readMockArgs(t, argsPath)
	if len(calls) < 3 {
		t.Fatalf("calls = %v", calls)
	}
	for _, c := range calls {
		if len(c) < 2 || c[0] != "-S" || c[1] != "/tmp/tmux-501/work" {
			t.Fatalf("call without -S <path>: %v", c)
		}
	}
	if !reflect.DeepEqual(calls[1][2:], []string{"break-pane", "-d", "-s", "%4", "-t", HiddenSessionName + ":"}) {
		t.Fatalf("break-pane call = %v", calls[1])
	}
	if !reflect.DeepEqual(calls[2][2:], []string{"join-pane", "-d", "-h", "-s", "%4", "-t", "@2"}) {
		t.Fatalf("join-pane call = %v", calls[2])
	}
}
