//go:build !windows

package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Unix-only test helpers. They drive a fake `claude` implemented as a POSIX
// shell script and reap real child processes via syscall, so they only build on
// non-Windows. The cross-platform fixtures live in helpers_test.go.

// writeFakeBin writes an executable shell script and returns its path.
func writeFakeBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return p
}

// writeMinimalProviders installs a providers.toml with one enabled provider under the
// test's XDG config dir.
func writeMinimalProviders(t *testing.T, xdg string) {
	t.Helper()
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deadReapedPID starts and waits for a trivial child, returning its
// now-dead-and-reaped pid (kill(pid,0) == ESRCH). Used to synthesize a
// "finished" background job without the zombie a Released-but-unwaited child
// would become in-test.
func deadReapedPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("/bin/sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Fatalf("run /bin/sh: %v", err)
	}
	return c.Process.Pid
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	// The grandchild writes asynchronously; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return -1
}

// waitGone polls pid for up to d and reports whether it is STILL alive
// (false = it died, which is what the no-orphan assertion wants).
func waitGone(pid int, d time.Duration) (stillAlive bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != syscall.ESRCH
}
