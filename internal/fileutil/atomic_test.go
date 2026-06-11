package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAtomicWrite_NewFile: writes to a non-existent path under an existing
// directory, then verifies the file exists with the requested mode + content
// and no orphan temp lingers.
func TestAtomicWrite_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	want := []byte(`{"hello":"world"}`)

	if err := AtomicWrite(path, want, 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Fatalf("content mismatch:\n got: %q\nwant: %q", got, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 { // no unix mode bits on windows
		t.Fatalf("mode = %o, want 0o600", st.Mode().Perm())
	}
	assertNoOrphanTemp(t, dir, "out.json")
}

// TestAtomicWrite_ReplaceExisting: overwrite an existing file. Pre-image must
// be replaced atomically (no truncation visible to a concurrent reader's
// stat).
func TestAtomicWrite_ReplaceExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(path, []byte("OLD-CONTENT"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	want := []byte("NEW-CONTENT")
	if err := AtomicWrite(path, want, 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("content = %q, want %q", got, want)
	}
	assertNoOrphanTemp(t, dir, "out.bin")
}

// TestAtomicWrite_ModeEnforced: a permissive umask must not relax 0o600.
func TestAtomicWrite_ModeEnforced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	// Some test environments inherit a permissive umask; force the
	// post-Chmod assertion to still hold.
	if err := AtomicWrite(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 { // no unix mode bits on windows
		t.Fatalf("mode = %o, want 0o600", st.Mode().Perm())
	}
}

// TestAtomicWrite_AlternateMode: ensures a non-0o600 mode (e.g. 0o644) is
// honored — the helper is not hard-coded to 0o600.
func TestAtomicWrite_AlternateMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache")
	if err := AtomicWrite(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o644 { // no unix mode bits on windows
		t.Fatalf("mode = %o, want 0o644", st.Mode().Perm())
	}
}

// TestAtomicWrite_MissingParent: CreateTemp into a non-existent directory must
// fail cleanly (the helper deliberately does not mkdir).
func TestAtomicWrite_MissingParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent-dir", "file") // dir is missing on purpose
	err := AtomicWrite(path, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("AtomicWrite into missing parent: want error, got nil")
	}
}

// TestAtomicWrite_EmptyPath: empty path is rejected explicitly so callers
// don't accidentally write to ".".
func TestAtomicWrite_EmptyPath(t *testing.T) {
	if err := AtomicWrite("", []byte("x"), 0o600); err == nil {
		t.Fatal("AtomicWrite with empty path: want error, got nil")
	}
}

// TestAtomicWrite_RenameFailureLeavesOriginalIntact: simulates the rename
// failing by setting the target to a path that can't be renamed onto (a
// directory). The pre-existing file at target must stay intact AND no orphan
// temp must remain.
func TestAtomicWrite_RenameFailureLeavesOriginalIntact(t *testing.T) {
	dir := t.TempDir()
	// target is a DIRECTORY: os.Rename of a file onto a non-empty directory
	// fails with EISDIR / ENOTEMPTY depending on OS. The contract here is
	// that AtomicWrite cleans up the temp and surfaces an error.
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	// Seed a file inside the directory so an accidental "overwrite" would be
	// observable as content change.
	if err := os.WriteFile(filepath.Join(target, "preserved"),
		[]byte("PRE"), 0o600); err != nil {
		t.Fatalf("seed preserved: %v", err)
	}
	err := AtomicWrite(target, []byte("NEW"), 0o600)
	if err == nil {
		t.Fatal("AtomicWrite onto a directory: want error, got nil")
	}
	got, err := os.ReadFile(filepath.Join(target, "preserved"))
	if err != nil {
		t.Fatalf("read preserved: %v", err)
	}
	if string(got) != "PRE" {
		t.Fatalf("preserved file content = %q, want PRE", got)
	}
	assertNoOrphanTemp(t, dir, "target")
}

// assertNoOrphanTemp checks that no leftover ".<base>.*.tmp" file remains in
// dir. Either the rename succeeded (temp gone) or the deferred cleanup ran on
// failure (temp gone).
func assertNoOrphanTemp(t *testing.T, dir, base string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing dir means the test never wrote anything; that's fine.
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == base {
			continue
		}
		// CreateTemp pattern: ".<base>.<random>.tmp"
		prefix := "." + base + "."
		if len(name) >= len(prefix) && name[:len(prefix)] == prefix &&
			len(name) >= 4 && name[len(name)-4:] == ".tmp" {
			t.Fatalf("orphan temp file remains: %s", name)
		}
	}
}
