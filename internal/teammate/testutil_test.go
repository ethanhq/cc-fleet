package teammate

// Shared hermetic test helpers for every test in this package.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/config"
)

// hermeticUnsetVars are the variables a surrounding Claude Code, tmux or iTerm2
// session leaks into `go test`. The teammate lane reads all of them, so
// hermeticHome removes them for the duration of the test.
var hermeticUnsetVars = []string{
	"TMUX", "TMUX_PANE", "TMUX_TMPDIR",
	"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID", "CLAUDE_CODE_ENTRYPOINT",
	EnvTeammateCommand, EnvAgentTeams, EnvSubagentModelForce, EnvProcessWrapper,
	"TERM_PROGRAM", "ITERM_SESSION_ID",
}

// testHome is the layout hermeticHome creates. Every directory exists and is
// empty; all paths are symlink-free (macOS temp dirs live behind /var → /private/var).
type testHome struct {
	Home       string // $HOME and %USERPROFILE%; profile.ProfilesDir() is <Home>/.claude/profiles
	ClaudeDir  string // $CLAUDE_CONFIG_DIR = claudepaths.Root(); deliberately NOT <Home>/.claude, so code that hardcodes ~/.claude is caught
	ConfigHome string // $XDG_CONFIG_HOME; config.ConfigDir() is <ConfigHome>/cc-fleet
	Bin        string // first entry on PATH: the only place a `claude` can be found (see writeFakeClaude)
}

// hermeticHome points HOME, USERPROFILE, CLAUDE_CONFIG_DIR and XDG_CONFIG_HOME at
// fresh temp directories, unsets hermeticUnsetVars, and sets PATH to Bin plus
// the system directories (/usr/bin:/bin:/usr/sbin:/sbin; on Windows Bin is
// prepended to the existing PATH), so the real ~/.local/bin/claude is never
// found. Everything is restored when the test ends. Uses t.Setenv, so it
// cannot be combined with t.Parallel.
func hermeticHome(t *testing.T) testHome {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("hermeticHome: resolve temp dir: %v", err)
	}
	h := testHome{
		Home:       filepath.Join(root, "home"),
		ClaudeDir:  filepath.Join(root, "claude-config"),
		ConfigHome: filepath.Join(root, "xdg-config"),
		Bin:        filepath.Join(root, "bin"),
	}
	for _, dir := range []string{h.Home, h.ClaudeDir, h.ConfigHome, h.Bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("hermeticHome: mkdir %s: %v", dir, err)
		}
	}
	t.Setenv("HOME", h.Home)
	t.Setenv("USERPROFILE", h.Home)
	t.Setenv("CLAUDE_CONFIG_DIR", h.ClaudeDir)
	t.Setenv("XDG_CONFIG_HOME", h.ConfigHome)
	path := h.Bin + string(os.PathListSeparator) + "/usr/bin:/bin:/usr/sbin:/sbin"
	if runtime.GOOS == "windows" {
		path = h.Bin + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	t.Setenv("PATH", path)
	for _, k := range hermeticUnsetVars {
		t.Setenv(k, "") // registers the restore of the original value
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("hermeticHome: unset %s: %v", k, err)
		}
	}
	return h
}

// writeScript writes an executable POSIX sh script at path: "#!/bin/sh\n" + body.
// It returns path. Fake executables are sh scripts, so a test that needs one
// is skipped on Windows.
func writeScript(t *testing.T, path, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake executables are POSIX sh scripts")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("writeScript: mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("writeScript: write %s: %v", path, err)
	}
	// WriteFile's mode is filtered by the umask; pin the x bits explicitly.
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("writeScript: chmod %s: %v", path, err)
	}
	return path
}

// writeFakeClaude writes a fake `claude` (a sh script with the given body) into
// dir and returns its path. With dir = hermeticHome(t).Bin it is the claude
// found on PATH.
func writeFakeClaude(t *testing.T, dir, body string) string {
	t.Helper()
	return writeScript(t, filepath.Join(dir, "claude"), body)
}

// TestHermeticHome checks the shared helpers themselves.
func TestHermeticHome(t *testing.T) {
	t.Setenv("CLAUDECODE", "1") // simulate a leak from the surrounding session
	h := hermeticHome(t)

	for k, want := range map[string]string{
		"HOME":              h.Home,
		"CLAUDE_CONFIG_DIR": h.ClaudeDir,
		"XDG_CONFIG_HOME":   h.ConfigHome,
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("$%s = %q, want %q", k, got, want)
		}
	}
	if h.ClaudeDir == filepath.Join(h.Home, ".claude") {
		t.Errorf("ClaudeDir must differ from <Home>/.claude")
	}
	if got := claudepaths.Root(); got != h.ClaudeDir {
		t.Errorf("claudepaths.Root() = %q, want %q", got, h.ClaudeDir)
	}
	if got, err := config.ConfigDir(); err != nil || got != filepath.Join(h.ConfigHome, "cc-fleet") {
		t.Errorf("config.ConfigDir() = %q, %v; want %q", got, err, filepath.Join(h.ConfigHome, "cc-fleet"))
	}
	for _, k := range hermeticUnsetVars {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("$%s still set (%q)", k, v)
		}
	}

	claude := writeFakeClaude(t, h.Bin, "echo '2.1.281 (Claude Code)'\n")
	found, err := exec.LookPath("claude")
	if err != nil || found != claude {
		t.Fatalf("LookPath(claude) = %q, %v; want the fake %q", found, err, claude)
	}
	out, err := exec.Command(found, "--version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "2.1.281 (Claude Code)" {
		t.Fatalf("fake claude --version = %q, %v", out, err)
	}
}
