//go:build !windows

package codexproxy

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

// The daemon must leave the launcher's session, not just its process group:
// killing the tmux pane hangs up the pane's session.
func TestDetachUsesSetsid(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	detach(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v, want Setsid only", cmd.SysProcAttr)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pid := cmd.Process.Pid
	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatal(err)
	}
	if sid != pid {
		t.Fatalf("child session %d, want its own (%d)", sid, pid)
	}
	if own, _ := unix.Getsid(0); own == sid {
		t.Fatal("child still shares the test's session")
	}
}
