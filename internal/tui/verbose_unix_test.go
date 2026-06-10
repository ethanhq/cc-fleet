//go:build !windows

package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOpenVerboseLog_PerPid0600Truncated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	f, path, err := openVerboseLog()
	if err != nil {
		t.Fatalf("openVerboseLog: %v", err)
	}
	if want := fmt.Sprintf("tui-verbose-%d.log", os.Getpid()); filepath.Base(path) != want {
		t.Fatalf("path %q, want basename %q", path, want)
	}
	if _, err := f.WriteString("session one\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %o, want 0600", fi.Mode().Perm())
	}

	// A second open of the same pid's path starts a fresh, empty trace.
	f2, _, err := openVerboseLog()
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if data, _ := os.ReadFile(path); len(data) != 0 {
		t.Fatalf("reopen did not start fresh: %q", data)
	}
}

// A pre-existing broad-mode file at this pid's path (its file is never swept; a
// pre-reboot pid's can linger) is replaced by a fresh private 0600 inode.
func TestOpenVerboseLog_ReplacesBroadFileWithPrivateInode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfgDir := filepath.Join(dir, "cc-fleet")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, fmt.Sprintf("tui-verbose-%d.log", os.Getpid()))
	if err := os.WriteFile(path, []byte("stale 0644 content"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, got, err := openVerboseLog()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got != path {
		t.Fatalf("path %q, want %q", got, path)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("reused log perm %o, want 0600", fi.Mode().Perm())
	}
}

// A symlink planted at this pid's path is unlinked, not followed: the symlink
// target is left untouched and a fresh regular file takes the path.
func TestOpenVerboseLog_DoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	cfgDir := filepath.Join(dir, "cc-fleet")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(target, []byte("do not truncate me"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfgDir, fmt.Sprintf("tui-verbose-%d.log", os.Getpid()))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	f, _, err := openVerboseLog()
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if data, _ := os.ReadFile(target); string(data) != "do not truncate me" {
		t.Fatalf("symlink target was truncated/followed: %q", data)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("path is still a symlink; want a fresh regular file")
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("fresh log perm %o, want 0600", fi.Mode().Perm())
	}
}

func TestSweepStaleVerboseLogs_DeadOnly(t *testing.T) {
	dir := t.TempDir()

	// A dead pid: run-and-reap a trivial child.
	c := exec.Command("/bin/sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := c.Process.Pid

	dead := filepath.Join(dir, fmt.Sprintf("tui-verbose-%d.log", deadPID))
	live := filepath.Join(dir, fmt.Sprintf("tui-verbose-%d.log", os.Getpid())) // this test process is alive
	other := filepath.Join(dir, "unrelated.log")
	for _, p := range []string{dead, live, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// selfPID=1 so the live file is judged purely by pid liveness, not self-skip.
	sweepStaleVerboseLogs(dir, 1)

	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("dead-pid log not swept (err=%v)", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live-pid log was swept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-matching file was touched: %v", err)
	}
}
