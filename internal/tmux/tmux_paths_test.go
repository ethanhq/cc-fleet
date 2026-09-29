//go:build !windows

package tmux

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// TestSocketPaths: only socket files under ${TMUX_TMPDIR}/tmux-<uid>/ are
// returned; regular files and subdirectories are not, and a missing directory
// yields nil.
func TestSocketPaths(t *testing.T) {
	// Unix socket paths are length-limited (104 bytes on darwin), so keep the
	// directory short instead of using t.TempDir.
	base, err := os.MkdirTemp("", "ccfsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	t.Setenv("TMUX_TMPDIR", base)

	if got := SocketPaths(); got != nil {
		t.Fatalf("missing dir: SocketPaths() = %v, want nil", got)
	}

	dir := filepath.Join(base, "tmux-"+strconv.Itoa(os.Getuid()))
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, name := range []string{"cc-fleet-swarm-a", "default"} {
		p := filepath.Join(dir, name)
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Fatalf("listen %s: %v", p, err)
		}
		t.Cleanup(func() { l.Close() })
		want = append(want, p)
	}
	if got := SocketPaths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SocketPaths() = %v, want %v", got, want)
	}
}

// TestNewServerPathUsesDashS: a path server prefixes every command with
// -S <path>; an empty path stays the bare default-server argv.
func TestNewServerPathUsesDashS(t *testing.T) {
	got := NewServerPath("/tmp/tmux-501/default").command("list-panes", "-a").Args
	want := []string{tmuxBinary, "-S", "/tmp/tmux-501/default", "list-panes", "-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("path server argv = %v, want %v", got, want)
	}
	got = NewServerPath("").command("list-panes").Args
	if want := []string{tmuxBinary, "list-panes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty path argv = %v, want %v", got, want)
	}
}

// TestListAllPanesParses feeds ':'-separated list-panes output through the
// exec seam (fake tmux on PATH) and checks the argv and every parsed field,
// including an empty @ccf_origin, a ':' inside the socket path and a skipped
// malformed line.
func TestListAllPanesParses(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "main:@1:%3:4242:0:::/tmp/tmux-501/default\n"+
		"claude-hidden:@7:%9:4343:1:2:@1:/tmp/tmux-501/default\n"+
		"x:@2:%4:4444:0:::/tmp/a:b/default\n"+
		"garbage line\n")

	got, err := NewServerPath("/tmp/tmux-501/default").ListAllPanes()
	if err != nil {
		t.Fatalf("ListAllPanes: %v", err)
	}
	want := []PaneInfo{
		{SocketPath: "/tmp/tmux-501/default", SessionName: "main", WindowID: "@1", PaneID: "%3", PanePID: 4242},
		{SocketPath: "/tmp/tmux-501/default", SessionName: "claude-hidden", WindowID: "@7", PaneID: "%9", PanePID: 4343,
			Dead: true, DeadStatus: 2, Origin: "@1"},
		{SocketPath: "/tmp/a:b/default", SessionName: "x", WindowID: "@2", PaneID: "%4", PanePID: 4444},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAllPanes = %+v\nwant %+v", got, want)
	}
	calls := readMockArgs(t, argsPath)
	wantArgs := []string{"-S", "/tmp/tmux-501/default", "list-panes", "-a", "-F", listAllPanesFormat}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], wantArgs) {
		t.Fatalf("argv = %v, want %v", calls, wantArgs)
	}
}

// TestListAllPanesDeadServer: a stale socket (tmux exits non-zero) is an
// error the caller can skip.
func TestListAllPanesDeadServer(t *testing.T) {
	mockTmux(t)
	t.Setenv("MOCK_EXIT_CODE", "1")
	if _, err := NewServerPath("/tmp/tmux-501/gone").ListAllPanes(); err == nil {
		t.Fatal("ListAllPanes on a dead server: want error")
	}
}

// TestCaptureJoinedArgs: the marker capture is exactly
// -S <path> capture-pane -p -J -S -200 -t %N.
func TestCaptureJoinedArgs(t *testing.T) {
	argsPath := mockTmux(t)
	setMockOutput(t, "joined line\n")

	out, err := NewServerPath("/tmp/tmux-501/default").CaptureJoined("%12", 200)
	if err != nil {
		t.Fatalf("CaptureJoined: %v", err)
	}
	if out != "joined line\n" {
		t.Fatalf("CaptureJoined output = %q", out)
	}
	calls := readMockArgs(t, argsPath)
	want := []string{"-S", "/tmp/tmux-501/default", "capture-pane", "-p", "-J", "-S", "-200", "-t", "%12"}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("argv = %v, want %v", calls, want)
	}
	if _, err := NewServerPath("/x").CaptureJoined("", 200); err == nil {
		t.Fatal("CaptureJoined with empty pane id: want error")
	}
}
