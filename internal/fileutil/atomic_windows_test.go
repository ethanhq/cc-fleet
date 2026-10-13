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
