//go:build !windows

package teardown

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnnotateHealth_PerPaneSocket: the captureFn seam receives (socket, paneID)
// so swarm panes can be captured on their private server. A default-server-only
// capture silently fails every swarm pane (marking them statusUnknown forever).
//
// The fake captureFn records what socket each invocation got, and we assert
// each teammate row's pane is captured on its OWN recorded socket.
func TestAnnotateHealth_PerPaneSocket(t *testing.T) {
	orig := captureFn
	t.Cleanup(func() { captureFn = orig })

	type call struct{ socket, pane string }
	var calls []call
	captureFn = func(socket, pane string) (string, error) {
		calls = append(calls, call{socket: socket, pane: pane})
		// Every pane succeeds with a clean banner; the test cares about argv,
		// not classification.
		return "● ok\n", nil
	}

	in := []Teammate{
		{Name: "alice", PaneID: "%10", Socket: ""},                     // in-tmux
		{Name: "bob", PaneID: "%20", Socket: "cc-fleet-swarm-teamA"},   // swarm A
		{Name: "carol", PaneID: "%30", Socket: "cc-fleet-swarm-teamB"}, // swarm B
	}
	out := AnnotateHealth(in)
	if len(out) != 3 {
		t.Fatalf("AnnotateHealth: got %d rows, want 3", len(out))
	}
	for i := range out {
		if out[i].Status != statusOK {
			t.Fatalf("row %d Status = %q, want ok", i, out[i].Status)
		}
	}
	wants := []call{
		{socket: "", pane: "%10"},
		{socket: "cc-fleet-swarm-teamA", pane: "%20"},
		{socket: "cc-fleet-swarm-teamB", pane: "%30"},
	}
	if len(calls) != len(wants) {
		t.Fatalf("captureFn calls = %d, want %d (%v)", len(calls), len(wants), calls)
	}
	for i := range calls {
		if calls[i] != wants[i] {
			t.Fatalf("call %d = %+v, want %+v", i, calls[i], wants[i])
		}
	}
}

// TestCapturePane_SocketScoped covers the real exec path: a non-empty socket
// MUST insert `-L <socket>` between tmuxBinary and the subcommand, scoping
// the capture to the right server. The fake tmux echoes its argv to a file
// so we can assert the wiring; without the -L the swarm pane silently
// reaches the default server and the swarm capture fails.
func TestCapturePane_SocketScoped(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	argsLog := filepath.Join(dir, "args.log")
	// The fake tmux records argv to MOCK_ARGS_FILE and echoes a fixed banner
	// so the caller's classification path (irrelevant to this test) is OK.
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$MOCK_ARGS_FILE\"; done\n" +
		"printf '__END__\\n' >> \"$MOCK_ARGS_FILE\"\n" +
		"printf 'banner\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsLog)

	const sock = "cc-fleet-swarm-cap"
	if _, err := capturePane(sock, "%77"); err != nil {
		t.Fatalf("capturePane(sock=%q): %v", sock, err)
	}
	// Read recorded argv and check the FIRST invocation began with -L <sock>.
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 2 || lines[0] != "-L" || lines[1] != sock {
		t.Fatalf("capturePane argv prefix = %v, want [-L %s ...]", lines, sock)
	}
}

// TestCapturePane_DefaultServer_NoDashL covers the in-tmux compatibility path:
// when socket=="" the argv MUST NOT include -L (tmux rejects an empty -L
// value, and the historical default-server callers are argv-exact tested).
func TestCapturePane_DefaultServer_NoDashL(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	argsLog := filepath.Join(dir, "args.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"$MOCK_ARGS_FILE\"; done\n" +
		"printf '__END__\\n' >> \"$MOCK_ARGS_FILE\"\n" +
		"printf 'banner\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsLog)

	if _, err := capturePane("", "%5"); err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read args.log: %v", err)
	}
	if strings.Contains(string(data), "-L") {
		t.Fatalf("default-server capturePane should not emit -L; argv=%q", string(data))
	}
}
