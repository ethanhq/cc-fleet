//go:build linux

package procintrospect

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStartUnixMilli_Btime: the jiffies token (USER_HZ 100) is added to
// /proc/stat's btime; without a btime line there is no conversion.
func TestStartUnixMilli_Btime(t *testing.T) {
	root := t.TempDir()
	orig := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = orig })

	stat := filepath.Join(root, "stat")
	if err := os.WriteFile(stat, []byte("cpu  1 2 3 4\nintr 5\nbtime 1790000000\nprocesses 42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ms, ok := StartUnixMilli("12345"); !ok || ms != 1_790_000_000_000+123_450 {
		t.Fatalf("StartUnixMilli(12345) = %d, %v; want %d", ms, ok, int64(1_790_000_000_000+123_450))
	}
	if err := os.WriteFile(stat, []byte("cpu  1 2 3 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ms, ok := StartUnixMilli("12345"); ok {
		t.Fatalf("StartUnixMilli without btime = %d, want !ok", ms)
	}
}
