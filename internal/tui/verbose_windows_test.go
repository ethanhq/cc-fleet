//go:build windows

package tui

import (
	"os"
	"os/exec"
	"testing"
)

func TestPidAliveForSweep_Windows(t *testing.T) {
	// This test process is alive.
	if !pidAliveForSweep(os.Getpid()) {
		t.Fatal("own pid judged dead")
	}

	// A reaped child no longer exists: OpenProcess on its pid fails with
	// ERROR_INVALID_PARAMETER → dead, so its log would be swept.
	c := exec.Command("cmd", "/c", "exit", "0")
	if err := c.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := c.Process.Pid
	if err := c.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	if pidAliveForSweep(pid) {
		t.Fatalf("reaped child pid %d judged alive", pid)
	}
}
