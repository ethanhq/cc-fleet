//go:build windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeFakeBinWindows writes a .cmd batch script acting as a fake claude and
// returns its path. The subagent execs it directly (PATHEXT-free, full path), so
// a .cmd is a valid argv[0] on Windows just as a #!/bin/sh script is on unix.
func writeFakeBinWindows(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

// readPIDWindows polls path until it holds a parseable pid (the grandchild
// records its pid asynchronously).
func readPIDWindows(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return -1
}

// TestRunClaude_TimeoutKillsProcessTree is the Windows grandchild-reap gate: a
// fake claude (.cmd) spawns a long-sleeping grandchild that records its pid, and
// the run hangs. When the deadline fires, runClaude's Job Object
// (TerminateJobObject via killGroupHard) must reap the WHOLE tree — the
// grandchild included — leaving no surviving pid. This is the real "done"
// criterion from the proposal; it only RUNS on a windows-latest runner.
func TestRunClaude_TimeoutKillsProcessTree(t *testing.T) {
	orig := waitGrace
	waitGrace = 500 * time.Millisecond
	t.Cleanup(func() { waitGrace = orig })

	gpidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// The batch leader spawns a detached grandchild: a child cmd that records its
	// own PID then sleeps for a long time via ping (a portable sleep). The leader
	// itself then sleeps. Only a tree kill reaps the grandchild — a kill of the
	// leader alone would leave it running.
	script := "@echo off\r\n" +
		"start /b cmd /c \"echo %^PID% > \"" + gpidFile + "\" & ping -n 60 127.0.0.1 > nul\"\r\n" +
		"ping -n 60 127.0.0.1 > nul\r\n"
	bin := writeFakeBinWindows(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, _, _ = runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "")
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("runClaude took %v with a sleeping grandchild; tree-kill model broken", elapsed)
	}

	gpid := readPIDWindows(t, gpidFile)
	if gpid <= 0 {
		t.Fatalf("grandchild never recorded its pid (%q)", gpidFile)
	}
	// The grandchild must be dead — TerminateJobObject reaps every process in the
	// job atomically.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(gpid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d survived the timeout — Job Object tree-kill missing", gpid)
}

// TestPidAlive_Windows sanity-checks the OpenProcess/GetExitCodeProcess liveness
// probe: the current process is alive; a clearly-invalid pid is dead.
func TestPidAlive_Windows(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Fatal("the current process must read as alive")
	}
	if pidAlive(-1) {
		t.Fatal("pid <= 0 must read as dead")
	}
}
