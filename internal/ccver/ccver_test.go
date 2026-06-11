package ccver

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		nums [3]int
	}{
		{"2.1.150", true, [3]int{2, 1, 150}},
		{"0.0.1", true, [3]int{0, 0, 1}},
		{"10.20.30", true, [3]int{10, 20, 30}},
		{"2.1", false, [3]int{}},
		{"2.1.150.4", false, [3]int{}},
		{"v2.1.150", false, [3]int{}},
		{"", false, [3]int{}},
		{"2.1.x", false, [3]int{}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseSemver(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseSemver(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.nums {
				t.Fatalf("parseSemver(%q) = %v, want %v", tc.in, got, tc.nums)
			}
		})
	}
}

func TestVersionFromPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/root/.local/share/claude/versions/2.1.150/claude", "2.1.150"},
		{"/usr/local/bin/claude-2.1.150", "2.1.150"},
		{"/usr/local/bin/claude", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := versionFromPath(tc.in); got != tc.want {
				t.Fatalf("versionFromPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		version, floor string
		want           bool
	}{
		{"2.1.88", "2.1.88", true},    // boundary: equal
		{"2.1.87", "2.1.88", false},   // below by patch
		{"2.1.89", "2.1.88", true},    // above by patch
		{"2.2.0", "2.1.88", true},     // above by minor
		{"3.0.0", "2.1.88", true},     // above by major
		{"1.9.99", "2.1.88", false},   // below by major
		{"", "2.1.88", false},         // empty version → unknown → false
		{"garbage", "2.1.88", false},  // unparseable → false
		{"v2.1.88", "2.1.88", false},  // leading v → unparseable
		{"2.1", "2.1.88", false},      // too few segments → unparseable
		{"2.1.88.4", "2.1.88", false}, // too many segments → unparseable
		{"2.1.88", "2.1", false},      // unparseable floor → false
	}
	for _, tc := range cases {
		t.Run(tc.version+"_vs_"+tc.floor, func(t *testing.T) {
			if got := AtLeast(tc.version, tc.floor); got != tc.want {
				t.Fatalf("AtLeast(%q, %q) = %v, want %v", tc.version, tc.floor, got, tc.want)
			}
		})
	}
}

func TestVersionForPath(t *testing.T) {
	// Layout basename resolves without exec.
	if got := VersionForPath("/root/.local/share/claude/versions/2.1.150/claude"); got != "2.1.150" {
		t.Fatalf("VersionForPath(layout) = %q, want 2.1.150", got)
	}
	// A path with no semver basename and no runnable binary → "".
	if got := VersionForPath(filepath.Join(t.TempDir(), "claude")); got != "" {
		t.Fatalf("VersionForPath(unknown) = %q, want \"\"", got)
	}
}

// TestDetect_PathLookup writes a fake `claude` executable into a tempdir,
// prepends it to PATH, and confirms Detect finds it. The fake doesn't need to
// be runnable for versionFromPath to succeed, but we name its parent dir with
// a semver so versionFromPath returns a value without exec.
func TestDetect_PathLookup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("writes a #!/bin/sh fake claude; windows layout discovery is covered by the _windows test")
	}
	dir := t.TempDir()
	verDir := filepath.Join(dir, "2.1.150")
	if err := os.MkdirAll(verDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bin := filepath.Join(verDir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write claude: %v", err)
	}
	t.Setenv("PATH", verDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	path, ver, err := Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if path != bin {
		// LookPath canonicalizes via PATH; the basename check is enough.
		if filepath.Base(path) != "claude" {
			t.Fatalf("path = %q, want one ending in claude", path)
		}
	}
	if ver != "2.1.150" {
		t.Fatalf("version = %q, want 2.1.150", ver)
	}
}

// writeExecClaude drops a runnable stub `claude` into versionsDir/<ver>/claude.
func writeExecClaude(t *testing.T, versionsDir, ver string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("writes a #!/bin/sh fake under HOME; windows layout discovery is covered by the _windows test")
	}
	d := filepath.Join(versionsDir, ver)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", d, err)
	}
	if err := os.WriteFile(filepath.Join(d, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write claude: %v", err)
	}
}

// TestDetect_VersionsLayoutFallback hides the binary from PATH and relies on
// ~/.local/share/claude/versions/<semver> discovery. Every candidate dir holds
// an executable claude, so the largest semver wins.
func TestDetect_VersionsLayoutFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// PATH must NOT contain a claude binary for this test to exercise the
	// fallback. Use a tmpdir that's guaranteed not to have one.
	t.Setenv("PATH", t.TempDir())

	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	for _, v := range []string{"1.2.3", "2.1.150", "2.1.99"} {
		writeExecClaude(t, versionsDir, v)
	}
	// A non-semver dir must be ignored entirely.
	if err := os.MkdirAll(filepath.Join(versionsDir, "not-a-version"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	path, ver, err := Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := filepath.Join(versionsDir, "2.1.150", "claude")
	if path != want {
		t.Fatalf("path = %q, want %q (largest semver)", path, want)
	}
	if ver != "2.1.150" {
		t.Fatalf("version = %q, want 2.1.150", ver)
	}
}

// TestDetect_SkipsVersionDirWithoutExecutable: locate() descends versions and
// commits to the FIRST dir that actually holds an executable claude. A
// higher-semver dir that's empty (or holds a non-executable file) is skipped,
// not blessed as the install.
func TestDetect_SkipsVersionDirWithoutExecutable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	// Highest semver: dir exists but NO executable claude inside.
	if err := os.MkdirAll(filepath.Join(versionsDir, "2.2.0"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A lower semver that DOES hold an executable claude.
	writeExecClaude(t, versionsDir, "2.1.0")

	path, ver, err := Detect()
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	want := filepath.Join(versionsDir, "2.1.0", "claude")
	if path != want {
		t.Fatalf("path = %q, want %q (skip empty 2.2.0, pick runnable 2.1.0)", path, want)
	}
	if ver != "2.1.0" {
		t.Fatalf("version = %q, want 2.1.0", ver)
	}
}

// TestDetect_AllVersionDirsEmpty_NotFound: when every versioned dir lacks an
// executable claude, Detect reports ErrNotFound rather than a phantom path.
func TestDetect_AllVersionDirsEmpty_NotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())

	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	for _, v := range []string{"2.2.0", "2.1.0"} {
		if err := os.MkdirAll(filepath.Join(versionsDir, v), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if _, _, err := Detect(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Detect err = %v, want ErrNotFound (no executable in any version dir)", err)
	}
}

func TestDetect_NotFound(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	_, _, err := Detect()
	if err == nil {
		t.Fatalf("Detect: want error when no claude is reachable")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestString(t *testing.T) {
	if got := String("/x/claude", "2.1.150"); got != "/x/claude (cc 2.1.150)" {
		t.Fatalf("String with version = %q", got)
	}
	if got := String("/x/claude", ""); got != "/x/claude (version unknown)" {
		t.Fatalf("String no version = %q", got)
	}
}
