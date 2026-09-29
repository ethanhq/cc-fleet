//go:build !windows

package claudebin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// binFake writes an executable sh script `claude` into a fresh dir. On
// --version it appends one line to countFile (when non-empty) and prints
// version. Returns the script path.
func binFake(t *testing.T, version, countFile string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n"
	if countFile != "" {
		script += "  echo x >> '" + countFile + "'\n"
	}
	script += "  echo '" + version + " (Claude Code)'\n  exit 0\nfi\nexit 0\n"
	binWrite(t, p, script, 0o755)
	return p
}

func binWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// binEnv gives the test a fresh HOME and a PATH holding only pathDir (plus the
// system dirs the sh fake needs). Returns the versions dir under HOME.
func binEnv(t *testing.T, pathDir string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	return filepath.Join(home, ".local", "share", "claude", "versions")
}

// TestResolveCachesVersion: the version of a resolved path is computed once per
// process — a repeat Resolve never re-runs `claude --version`.
func TestResolveCachesVersion(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	fake := binFake(t, "2.1.150", count)
	binEnv(t, filepath.Dir(fake))

	for i := 0; i < 3; i++ {
		path, ver, err := Resolve()
		if err != nil {
			t.Fatalf("call %d: Resolve: %v", i, err)
		}
		if path != fake || ver != "2.1.150" {
			t.Fatalf("call %d = (%q, %q), want (%q, 2.1.150)", i, path, ver, fake)
		}
	}
	data, err := os.ReadFile(count)
	if err != nil {
		t.Fatalf("read count: %v", err)
	}
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Fatalf("--version ran %d times, want 1 (cached per path)", n)
	}
}

// TestResolveRejectsNonExecutable: a 0-byte claude on PATH and a non-executable
// flat versions file are both refused with ErrNotFound.
func TestResolveRejectsNonExecutable(t *testing.T) {
	t.Run("zero-byte on PATH", func(t *testing.T) {
		dir := t.TempDir()
		binWrite(t, filepath.Join(dir, "claude"), "", 0o755)
		binEnv(t, dir)
		if _, _, err := Resolve(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve err = %v, want ErrNotFound", err)
		}
	})
	t.Run("non-executable versions file", func(t *testing.T) {
		vd := binEnv(t, t.TempDir())
		binWrite(t, filepath.Join(vd, "2.1.150"), "#!/bin/sh\n", 0o644)
		if _, _, err := Resolve(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve err = %v, want ErrNotFound", err)
		}
	})
	t.Run("nothing installed", func(t *testing.T) {
		binEnv(t, t.TempDir())
		_, _, err := Resolve()
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Resolve err = %v, want ErrNotFound", err)
		}
	})
}

// TestForSessionPrefersVersionsFile: the session's exact version under the
// per-version layout wins over the PATH claude.
func TestForSessionPrefersVersionsFile(t *testing.T) {
	fake := binFake(t, "2.1.999", "")
	vd := binEnv(t, filepath.Dir(fake))
	want := filepath.Join(vd, "2.1.150")
	binWrite(t, want, "#!/bin/sh\nexit 0\n", 0o755)

	got, err := ForSession("2.1.150")
	if err != nil {
		t.Fatalf("ForSession: %v", err)
	}
	if got != want {
		t.Fatalf("ForSession = %q, want %q", got, want)
	}
}

// TestForSessionFallsBackToResolve: an empty, missing, unrunnable or path-unsafe
// version falls back to Resolve's path; with no claude at all the error is
// ErrNotFound.
func TestForSessionFallsBackToResolve(t *testing.T) {
	fake := binFake(t, "2.1.999", "")
	vd := binEnv(t, filepath.Dir(fake))
	binWrite(t, filepath.Join(vd, "2.1.283"), "", 0o755)                     // 0-byte leftover
	binWrite(t, filepath.Join(vd, "2.1.100"), "#!/bin/sh\n", 0o644)          // no x bit
	binWrite(t, filepath.Join(vd, "2.1.50", "claude"), "#!/bin/sh\n", 0o755) // a dir, not a file

	for _, v := range []string{"", "9.9.9", "2.1.283", "2.1.100", "2.1.50", "../versions/2.1.50", ".."} {
		got, err := ForSession(v)
		if err != nil {
			t.Fatalf("ForSession(%q): %v", v, err)
		}
		if got != fake {
			t.Fatalf("ForSession(%q) = %q, want the Resolve path %q", v, got, fake)
		}
	}

	binEnv(t, t.TempDir())
	if _, err := ForSession("2.1.150"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ForSession with no claude: err = %v, want ErrNotFound", err)
	}
}
