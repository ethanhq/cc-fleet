package fingerprint

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// realProbeCmdline / realProbeEnviron are the exact NUL-separated byte
// streams that a real native Agent teammate process produced. Keeping them
// verbatim — including the trailing NUL that /proc always appends — protects
// against silent regressions in splitNul / templatize.
//
// The cmdline carries `--model claude-opus-4-7` so templatize() can be
// verified to strip it.
var realProbeCmdline = []byte(
	"/root/.local/share/claude/versions/2.1.150\x00" +
		"--agent-id\x00probe1@probe-native\x00" +
		"--agent-name\x00probe1\x00" +
		"--team-name\x00probe-native\x00" +
		"--agent-color\x00blue\x00" +
		"--parent-session-id\x00bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb\x00" +
		"--agent-type\x00general-purpose\x00" +
		"--dangerously-skip-permissions\x00" +
		"--model\x00claude-opus-4-7\x00",
)

var realProbeEnviron = []byte(
	"CLAUDECODE=1\x00" +
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1\x00" +
		"PATH=/usr/bin:/bin\x00" +
		"HOME=/root\x00" +
		"SHELL=/bin/zsh\x00" +
		"USER=root\x00" +
		"PWD=/root\x00",
)

// writeMockProc materialises the mock cmdline + environ inside dir and returns
// their paths.
func writeMockProc(t *testing.T, dir string, cmdline, environ []byte) (string, string) {
	t.Helper()
	cmd := filepath.Join(dir, "cmdline")
	env := filepath.Join(dir, "environ")
	if err := os.WriteFile(cmd, cmdline, 0o600); err != nil {
		t.Fatalf("write cmdline: %v", err)
	}
	if err := os.WriteFile(env, environ, 0o600); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	return cmd, env
}

func TestCaptureFromFiles_RealProbeSample(t *testing.T) {
	cmd, env := writeMockProc(t, t.TempDir(), realProbeCmdline, realProbeEnviron)

	fp, err := CaptureFromFiles(cmd, env)
	if err != nil {
		t.Fatalf("CaptureFromFiles: %v", err)
	}

	// 1. binary_path is argv[0] verbatim.
	if want := "/root/.local/share/claude/versions/2.1.150"; fp.BinaryPath != want {
		t.Fatalf("BinaryPath = %q, want %q", fp.BinaryPath, want)
	}

	// 2. cc_version pulled from the basename, not from --version exec.
	if fp.CCVersion != "2.1.150" {
		t.Fatalf("CCVersion = %q, want 2.1.150", fp.CCVersion)
	}

	// 3. Env is restricted to the allowlist (CLAUDECODE +
	//    CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS). Everything else dropped.
	wantEnv := map[string]string{
		"CLAUDECODE":                           "1",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
	}
	if !reflect.DeepEqual(fp.Env, wantEnv) {
		t.Fatalf("Env = %v, want %v", fp.Env, wantEnv)
	}
	for _, banned := range []string{"PATH", "HOME", "SHELL", "USER", "PWD"} {
		if _, ok := fp.Env[banned]; ok {
			t.Fatalf("env %q leaked into fingerprint", banned)
		}
	}

	// 4. flags_template carries placeholders, not the concrete values.
	wantFlags := []string{
		"--agent-id", "{name}@{team}",
		"--agent-name", "{name}",
		"--team-name", "{team}",
		"--agent-color", "{color}",
		"--parent-session-id", "{lead_session_id}",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
	}
	if !reflect.DeepEqual(fp.FlagsTemplate, wantFlags) {
		t.Fatalf("FlagsTemplate mismatch:\n got: %v\nwant: %v", fp.FlagsTemplate, wantFlags)
	}

	// 5. --model + its value must be gone — cc-fleet sets that itself.
	for _, tok := range fp.FlagsTemplate {
		if tok == "--model" {
			t.Fatalf("FlagsTemplate should not contain --model: %v", fp.FlagsTemplate)
		}
		if tok == "claude-opus-4-7" {
			t.Fatalf("FlagsTemplate should not contain claude-opus-4-7: %v", fp.FlagsTemplate)
		}
	}

	// 6. CapturedAt is set (we don't pin the exact value, just non-zero).
	if fp.CapturedAt.IsZero() {
		t.Fatal("CapturedAt is zero")
	}
}

func TestCaptureFromFiles_EmptyCmdline(t *testing.T) {
	cmd, env := writeMockProc(t, t.TempDir(), []byte{}, realProbeEnviron)
	_, err := CaptureFromFiles(cmd, env)
	if err == nil {
		t.Fatal("CaptureFromFiles: want error on empty cmdline, got nil")
	}
	if !strings.Contains(err.Error(), "cmdline is empty") {
		t.Fatalf("error %q should mention empty cmdline", err)
	}
}

func TestCaptureFromFiles_MissingCmdline(t *testing.T) {
	dir := t.TempDir()
	// Only environ exists; cmdline path is never created.
	env := filepath.Join(dir, "environ")
	if err := os.WriteFile(env, realProbeEnviron, 0o600); err != nil {
		t.Fatalf("write environ: %v", err)
	}
	_, err := CaptureFromFiles(filepath.Join(dir, "nope"), env)
	if err == nil {
		t.Fatal("CaptureFromFiles: want error on missing cmdline, got nil")
	}
}

func TestCaptureFromFiles_VersionFallbackTriggered(t *testing.T) {
	// binary_path has no semver suffix → versionFromBinaryPath returns "" →
	// we fall through to versionFromBinaryExec, which fails for a fake path,
	// so the overall capture errors. This locks in the "no silent empty
	// cc_version" behaviour.
	cmdline := []byte(
		"/usr/local/bin/claude-no-version-suffix\x00" +
			"--agent-id\x00x@y\x00" +
			"--agent-name\x00x\x00" +
			"--team-name\x00y\x00",
	)
	cmd, env := writeMockProc(t, t.TempDir(), cmdline, realProbeEnviron)

	_, err := CaptureFromFiles(cmd, env)
	if err == nil {
		t.Fatal("CaptureFromFiles: want error when cc_version unresolvable, got nil")
	}
	if !strings.Contains(err.Error(), "cc_version") {
		t.Fatalf("error %q should mention cc_version", err)
	}
}

func TestCapture_TemplatizeIdempotent(t *testing.T) {
	// Running templatize() on already-templatized args must be a no-op (used
	// indirectly by Apply round-trips during testing).
	in := []string{
		"--agent-id", "{name}@{team}",
		"--agent-name", "{name}",
		"--team-name", "{team}",
		"--agent-color", "{color}",
		"--parent-session-id", "{lead_session_id}",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
	}
	got := templatize(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("templatize not idempotent:\n got: %v\nwant: %v", got, in)
	}
}

// TestCapture_TemplatizeStripsSettings: a fingerprint accidentally captured from
// a PROVIDER teammate (not a native Agent probe) carries `--settings <provider>.json`.
// templatize MUST strip it (like --model) — otherwise
// that provider's profile is frozen into the "native" template, lands first on every
// later spawn, and the request hits the wrong provider's endpoint carrying another
// provider's model → "model not found" for all non-matching providers. A doubly-tainted
// capture (two --settings) must also be fully cleaned.
func TestCapture_TemplatizeStripsSettings(t *testing.T) {
	in := []string{
		"--agent-id", "w1@team",
		"--agent-name", "w1",
		"--team-name", "team",
		"--agent-color", "blue",
		"--parent-session-id", "sess",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
		"--settings", "/root/.claude/profiles/glm.json",
		"--settings", "/root/.claude/profiles/deepseek.json", // doubly tainted
		"--model", "glm-4.7",
	}
	want := []string{
		"--agent-id", "{name}@{team}",
		"--agent-name", "{name}",
		"--team-name", "{team}",
		"--agent-color", "{color}",
		"--parent-session-id", "{lead_session_id}",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
	}
	got := templatize(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("templatize did not strip --settings/--model:\n got: %v\nwant: %v", got, want)
	}
	for _, a := range got {
		if a == "--settings" {
			t.Fatal("--settings survived templatize — provider profile would taint every spawn")
		}
	}
}

func TestVersionFromBinaryPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/root/.local/share/claude/versions/2.1.150", "2.1.150"},
		{"/opt/claude/versions/10.0.42", "10.0.42"},
		{"/usr/bin/claude", ""},
		{"", ""},
		{"/x/y/2.1.150-beta", ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := versionFromBinaryPath(tc.path); got != tc.want {
				t.Fatalf("versionFromBinaryPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestSplitNul(t *testing.T) {
	in := []byte("a\x00bb\x00ccc\x00")
	got := splitNul(in)
	want := []string{"a", "bb", "ccc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitNul = %v, want %v", got, want)
	}
	if got := splitNul(nil); got != nil {
		t.Fatalf("splitNul(nil) = %v, want nil", got)
	}
}

func TestFilterEnv(t *testing.T) {
	in := []string{
		"CLAUDECODE=1",
		"FOO=bar",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"=malformed",
		"NOEQUAL",
	}
	got := filterEnv(in, []string{"CLAUDECODE", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"})
	want := map[string]string{
		"CLAUDECODE":                           "1",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filterEnv = %v, want %v", got, want)
	}
}

func TestRemoveFlagPair(t *testing.T) {
	in := []string{"--keep", "1", "--model", "claude-opus-4-7", "--also-keep", "2"}
	got := removeFlagPair(in, "--model")
	want := []string{"--keep", "1", "--also-keep", "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removeFlagPair = %v, want %v", got, want)
	}
}
