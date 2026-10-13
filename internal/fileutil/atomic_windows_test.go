//go:build windows

package fileutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAtomicWriteReplacesOpenTarget: a reader briefly holding the target open (os.Open grants
// no FILE_SHARE_DELETE) delays the replace instead of failing it — callers such as the job-meta
// hold protocol drop a failed write.
func TestAtomicWriteReplacesOpenTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, func() { f.Close() })
	if err := AtomicWrite(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("AtomicWrite over an open target: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}
}

// TestRenameRetryWaitsForOpenTarget pins the real error without a timer: the replace a reader
// blocks fails with one isRenameBlocked re-tries, and goes through once the reader closes.
func TestRenameRetryWaitsForOpenTarget(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "meta.json.tmp"), filepath.Join(dir, "meta.json")
	for _, p := range []string{from, to} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(to)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	calls := 0
	err = retryTransientRename(func() error {
		calls++
		err := os.Rename(from, to)
		if calls == 1 {
			if !isRenameBlocked(err) {
				t.Errorf("rename over an open target: err=%v, want a blocked replace", err)
			}
			f.Close()
		}
		return err
	}, isRenameBlocked, renameAttempts, renameDelay)
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want nil after one retry", err, calls)
	}
}
