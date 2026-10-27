//go:build !windows

package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCodex writes an sh `codex` at path that prints out on --version and
// appends a line to count per call. Returns count.
func fakeCodex(t *testing.T, path, out string) string {
	t.Helper()
	count := filepath.Join(t.TempDir(), "count")
	mustWrite(t, path, "#!/bin/sh\necho x >> '"+count+"'\necho '"+out+"'\n", 0o755)
	return count
}

func versionCalls(t *testing.T, count string) int {
	t.Helper()
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x")
}

// TestCodexResolveFindsVersionOnce: the binary on PATH is returned with the
// version parsed from `codex --version`, which runs once per path per process.
func TestCodexResolveFindsVersionOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	bin := filepath.Join(dir, "codex")
	count := fakeCodex(t, bin, "codex-cli 0.163.0")

	for i := 0; i < 3; i++ {
		path, ver, err := codexEngine{}.Resolve()
		if err != nil || path != bin || ver != "0.163.0" {
			t.Fatalf("Resolve = (%q, %q, %v), want (%q, 0.163.0, nil)", path, ver, err, bin)
		}
	}
	if n := versionCalls(t, count); n != 1 {
		t.Fatalf("--version ran %d times, want 1", n)
	}
}

// TestCodexResolveNpmSymlink: npm puts a symlink to codex.js on PATH; Resolve
// returns the link, not its target.
func TestCodexResolveNpmSymlink(t *testing.T) {
	root := t.TempDir()
	fakeCodex(t, filepath.Join(root, "lib", "codex.js"), "codex-cli 0.163.0")
	link := filepath.Join(root, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "lib", "codex.js"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(link))

	path, ver, err := codexEngine{}.Resolve()
	if err != nil || path != link || ver != "0.163.0" {
		t.Fatalf("Resolve = (%q, %q, %v), want (%q, 0.163.0, nil)", path, ver, err, link)
	}
}

// TestCodexResolveUnknownVersion: output without a plain X.Y.Z (garbage, or a
// pre-release) resolves with version "" and no error, and is re-probed next time.
func TestCodexResolveUnknownVersion(t *testing.T) {
	for _, out := range []string{"codex-cli dev", "codex-cli 0.164.0-alpha.1"} {
		t.Run(out, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			bin := filepath.Join(dir, "codex")
			count := fakeCodex(t, bin, out)

			for i := 0; i < 2; i++ {
				path, ver, err := codexEngine{}.Resolve()
				if err != nil || path != bin || ver != "" {
					t.Fatalf("Resolve = (%q, %q, %v), want (%q, \"\", nil)", path, ver, err, bin)
				}
			}
			if n := versionCalls(t, count); n != 2 {
				t.Fatalf("--version ran %d times, want 2 (an unknown version is not cached)", n)
			}
		})
	}
}

// TestCodexResolveRelativePath: a relative PATH hit is rejected by default
// (ErrDot) but accepted under GODEBUG=execerrdot=0, where Resolve must
// absolutize it so cmd.Dir and the version cache key stay cwd-independent.
func TestCodexResolveRelativePath(t *testing.T) {
	root := t.TempDir()
	fakeCodex(t, filepath.Join(root, "tools", "bin", "codex"), "codex-cli 0.163.0")
	t.Chdir(root)
	t.Setenv("PATH", filepath.Join("tools", "bin"))
	if _, _, err := (codexEngine{}).Resolve(); !errors.Is(err, errCodexNotFound) {
		t.Fatalf("relative PATH hit: err = %v, want errCodexNotFound", err)
	}

	t.Setenv("GODEBUG", "execerrdot=0")
	want, err := filepath.Abs(filepath.Join("tools", "bin", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	path, ver, err := codexEngine{}.Resolve()
	if err != nil || path != want || ver != "0.163.0" || !filepath.IsAbs(path) {
		t.Fatalf("Resolve = (%q, %q, %v), want (%q, 0.163.0, nil)", path, ver, err, want)
	}
}

// TestCodexResolveNotFound: no codex on PATH, or a 0-byte one LookPath still
// accepts, both fail with errCodexNotFound.
func TestCodexResolveNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, _, err := (codexEngine{}).Resolve(); !errors.Is(err, errCodexNotFound) {
		t.Fatalf("no codex: err = %v, want errCodexNotFound", err)
	}

	dir := t.TempDir()
	t.Setenv("PATH", dir)
	mustWrite(t, filepath.Join(dir, "codex"), "", 0o755)
	if _, _, err := (codexEngine{}).Resolve(); !errors.Is(err, errCodexNotFound) {
		t.Fatalf("0-byte codex: err = %v, want errCodexNotFound", err)
	}
}
