//go:build !windows

package ccver

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// binWriteFile writes a file with the given content and mode (chmod after the
// write so the umask cannot strip bits).
func binWriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// binLayoutHome points HOME at a fresh temp dir, empties PATH of any claude and
// returns the versions dir of the per-version layout.
func binLayoutHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	return filepath.Join(home, ".local", "share", "claude", "versions")
}

// TestLocatedFlatVersionsLayout covers the flat layout (versions/<semver> is the
// binary itself), symlinks judged by their target, 0-byte and non-executable
// files skipped, and semver-descending selection (TM-8).
func TestLocatedFlatVersionsLayout(t *testing.T) {
	const script = "#!/bin/sh\nexit 0\n"

	t.Run("flat file", func(t *testing.T) {
		vd := binLayoutHome(t)
		binWriteFile(t, filepath.Join(vd, "2.1.280"), script, 0o755)
		path, ver, err := Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if want := filepath.Join(vd, "2.1.280"); path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
		if ver != "2.1.280" {
			t.Fatalf("version = %q, want 2.1.280", ver)
		}
	})

	t.Run("zero-byte newest skipped", func(t *testing.T) {
		vd := binLayoutHome(t)
		binWriteFile(t, filepath.Join(vd, "2.1.283"), "", 0o755) // mid-download leftover
		binWriteFile(t, filepath.Join(vd, "2.1.281"), script, 0o755)
		path, ok := Located()
		if !ok {
			t.Fatal("Located() = false; want the runnable 2.1.281")
		}
		if want := filepath.Join(vd, "2.1.281"); path != want {
			t.Fatalf("path = %q, want %q (0-byte 2.1.283 skipped)", path, want)
		}
	})

	t.Run("non-executable newest skipped", func(t *testing.T) {
		vd := binLayoutHome(t)
		binWriteFile(t, filepath.Join(vd, "2.2.0"), script, 0o644)
		binWriteFile(t, filepath.Join(vd, "2.1.0"), script, 0o755)
		path, ok := Located()
		if !ok {
			t.Fatal("Located() = false; want the runnable 2.1.0")
		}
		if want := filepath.Join(vd, "2.1.0"); path != want {
			t.Fatalf("path = %q, want %q (non-executable 2.2.0 skipped)", path, want)
		}
	})

	t.Run("symlink to executable", func(t *testing.T) {
		vd := binLayoutHome(t)
		target := filepath.Join(t.TempDir(), "claude-real")
		binWriteFile(t, target, script, 0o755)
		if err := os.MkdirAll(vd, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(vd, "2.1.300")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		binWriteFile(t, filepath.Join(vd, "2.1.200"), script, 0o755)
		path, ver, err := Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if path != link {
			t.Fatalf("path = %q, want the symlink %q (followed by os.Stat)", path, link)
		}
		if ver != "2.1.300" {
			t.Fatalf("version = %q, want 2.1.300", ver)
		}
	})

	t.Run("dangling or bad symlink skipped", func(t *testing.T) {
		vd := binLayoutHome(t)
		if err := os.MkdirAll(vd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(vd, "2.9.0")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		empty := filepath.Join(t.TempDir(), "empty")
		binWriteFile(t, empty, "", 0o755)
		if err := os.Symlink(empty, filepath.Join(vd, "2.8.0")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		binWriteFile(t, filepath.Join(vd, "2.1.0"), script, 0o755)
		path, ok := Located()
		if !ok {
			t.Fatal("Located() = false; want the runnable 2.1.0")
		}
		if want := filepath.Join(vd, "2.1.0"); path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
	})

	t.Run("semver descending across layouts", func(t *testing.T) {
		vd := binLayoutHome(t)
		binWriteFile(t, filepath.Join(vd, "2.1.9"), script, 0o755)            // flat
		binWriteFile(t, filepath.Join(vd, "2.1.10", "claude"), script, 0o755) // dir
		binWriteFile(t, filepath.Join(vd, "1.99.99"), script, 0o755)
		path, ver, err := Detect()
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if want := filepath.Join(vd, "2.1.10", "claude"); path != want {
			t.Fatalf("path = %q, want %q (2.1.10 > 2.1.9 numerically)", path, want)
		}
		if ver != "2.1.10" {
			t.Fatalf("version = %q, want 2.1.10", ver)
		}
	})

	t.Run("only bad files", func(t *testing.T) {
		vd := binLayoutHome(t)
		binWriteFile(t, filepath.Join(vd, "2.1.283"), "", 0o755)
		binWriteFile(t, filepath.Join(vd, "2.1.282"), script, 0o600)
		if _, _, err := Detect(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Detect err = %v, want ErrNotFound", err)
		}
	})
}

// TestIsExecutableUnix locks the unix rule: non-empty regular file with an x bit,
// symlinks followed.
func TestIsExecutableUnix(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	binWriteFile(t, good, "x", 0o755)
	noX := filepath.Join(dir, "nox")
	binWriteFile(t, noX, "x", 0o644)
	empty := filepath.Join(dir, "empty")
	binWriteFile(t, empty, "", 0o755)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{good: true, link: true, noX: false, empty: false, dir: false, filepath.Join(dir, "missing"): false}
	for p, want := range cases {
		if got := IsExecutable(p); got != want {
			t.Errorf("IsExecutable(%q) = %v, want %v", p, got, want)
		}
	}
}
