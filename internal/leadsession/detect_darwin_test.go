//go:build darwin

package leadsession

import (
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestDarwin_DetectFromPID_RealChild is the on-device proof that the darwin
// ancestor-walk + PID-reuse guard work against a REAL live process via `ps`
// (no /proc). It starts a child, authors a Claude session file for that child's
// pid whose procStart is the UTC date string Claude itself would write, and
// asserts DetectFromPID validates it — exercising the parentPID-less direct hit
// (sessionIDForPID), the darwin procStart (`ps -o lstart=` → epoch), and
// normalizeFileProcStart (UTC date string → epoch).
//
// A Bash-tool-launched process is reparented to launchd, severing ancestry to
// the real Claude session, so a full spawn→lead-flag e2e falls to frozen-template
// regardless of platform. This unit proves the darwin mechanism end-to-end.
func TestDarwin_DetectFromPID_RealChild(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HOME", t.TempDir()) // never fall through to a real ~/.claude

	// The child's real start time, via the SAME production reader the guard uses
	// (darwin: `ps -o lstart=` parsed to epoch seconds).
	epochStr, ok := procStart(pid)
	if !ok {
		t.Fatalf("procStart(%d) failed on darwin", pid)
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		t.Fatalf("procStart returned non-epoch %q: %v", epochStr, err)
	}
	// Claude stores procStart as a UTC date string; normalizeFileProcStart maps
	// it back to the same epoch the darwin reader produces.
	utc := time.Unix(epoch, 0).UTC().Format("Mon Jan _2 15:04:05 2006")

	writeSession(t, cfg, pid, "darwin-session", utc)
	if got := DetectFromPID(pid); got != "darwin-session" {
		t.Fatalf("DetectFromPID(%d) = %q, want darwin-session (darwin ps walk + epoch procStart match)", pid, got)
	}

	// Negative: a procStart one second off (a recycled-PID stand-in) must fail
	// closed — never attribute a stale session to a live pid.
	mismatch := time.Unix(epoch+1, 0).UTC().Format("Mon Jan _2 15:04:05 2006")
	writeSession(t, cfg, pid, "stale-session", mismatch)
	if got := DetectFromPID(pid); got != "" {
		t.Fatalf("DetectFromPID(%d) with mismatched procStart = %q, want \"\" (fail closed)", pid, got)
	}
}
