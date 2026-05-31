//go:build linux || darwin

package procintrospect

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// startSleeper launches `sleep <marker>` as a child of this test process and
// returns its pid. The (unusual, large) duration doubles as a recognizable
// marker token in the child's argv so Cmdline/ProcessTable assertions can find
// it. The child is killed + reaped via t.Cleanup.
func startSleeper(t *testing.T, marker string) int {
	t.Helper()
	cmd := exec.Command("sleep", marker)
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep (no /bin/sleep?): %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// TestCmdline_RealChild verifies Cmdline returns the child's argv on the current
// OS (Linux /proc, darwin ps). The marker (a long, unusual sleep duration) must
// appear as an argv token, proving the reader didn't truncate or mis-split it.
func TestCmdline_RealChild(t *testing.T) {
	const marker = "31847"
	pid := startSleeper(t, marker)

	var argv []string
	for i := 0; i < 40; i++ { // ps/proc can lag a freshly-forked child briefly
		if a, err := Cmdline(pid); err == nil && len(a) >= 2 {
			argv = a
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(argv) < 2 {
		t.Fatalf("Cmdline(%d) = %v, want at least [sleep %s]", pid, argv, marker)
	}
	found := false
	for _, a := range argv {
		if a == marker {
			found = true
		}
	}
	if !found {
		t.Fatalf("Cmdline(%d) = %v, want it to contain marker %q", pid, argv, marker)
	}
}

// TestProcessTable_IncludesChild verifies the whole-table scan (used by the
// spawn rollback reap + teardown ghost reap) includes a live child with its argv.
func TestProcessTable_IncludesChild(t *testing.T) {
	const marker = "31849"
	pid := startSleeper(t, marker)

	var found bool
	for i := 0; i < 40; i++ {
		procs, err := ProcessTable()
		if err != nil {
			t.Fatalf("ProcessTable: %v", err)
		}
		for _, p := range procs {
			if p.PID == pid {
				found = true
				// argv should carry the marker too.
				ok := false
				for _, a := range p.Argv {
					if a == marker {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("ProcessTable entry for %d = %v, want marker %q", pid, p.Argv, marker)
				}
				break
			}
		}
		if found {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !found {
		t.Fatalf("ProcessTable did not include child pid %d", pid)
	}
}

// TestChildren_RealChild verifies Children lists a freshly-forked direct child
// (Linux /proc task children, darwin pgrep -P).
func TestChildren_RealChild(t *testing.T) {
	const marker = "31851"
	child := startSleeper(t, marker)
	parent := os.Getpid()

	var kids []int
	for i := 0; i < 40; i++ {
		kids = Children(parent)
		if contains(kids, child) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !contains(kids, child) {
		t.Fatalf("Children(%d) = %v, want it to include direct child %d", parent, kids, child)
	}
}

// TestCmdline_GonePID returns no argv for a pid that cannot exist, mirroring the
// missing-/proc-entry behavior the callers rely on (best-effort, never panics).
func TestCmdline_GonePID(t *testing.T) {
	const impossible = 1<<30 + 7 // far above any real pid on Linux/darwin
	if argv, err := Cmdline(impossible); err == nil && len(argv) > 0 {
		t.Fatalf("Cmdline(%d) = %v, want empty/err for a non-existent pid", impossible, argv)
	}
}

// TestChildren_GonePID returns no children for a pid that cannot exist.
func TestChildren_GonePID(t *testing.T) {
	const impossible = 1<<30 + 9
	if kids := Children(impossible); len(kids) != 0 {
		t.Fatalf("Children(%d) = %v, want empty for a non-existent pid", impossible, kids)
	}
}
