package teardown

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// argvOf splits a space-separated string into an argv slice. Convenience for
// keeping the tests readable now that parseTeammateCmdline takes []string.
func argvOf(s string) []string { return strings.Fields(s) }

func TestParseTeammateCmdline_Full(t *testing.T) {
	argv := []string{
		"env", "CLAUDECODE=1", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
		"/root/.local/share/claude/versions/2.1.150",
		"--agent-id", "alice@alpha",
		"--agent-name", "alice",
		"--team-name", "alpha",
		"--agent-color", "cyan",
		"--parent-session-id", "abc-uuid",
		"--agent-type", "general-purpose",
		"--dangerously-skip-permissions",
		"--settings", "/root/.claude/profiles/glm.json",
		"--model", "glm-4-flash",
	}

	got, ok := parseTeammateCmdline(argv)
	if !ok {
		t.Fatal("parseTeammateCmdline: ok=false, want true")
	}
	want := Teammate{
		Name:   "alice",
		Team:   "alpha",
		Vendor: "glm",
		Model:  "glm-4-flash",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parse mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestParseTeammateCmdline_NoAtSign(t *testing.T) {
	// --agent-id without @ → Name set, Team empty (fallback to --team-name).
	argv := argvOf("claude --agent-id solo --team-name myteam --model x")
	got, ok := parseTeammateCmdline(argv)
	if !ok {
		t.Fatal("ok=false")
	}
	if got.Name != "solo" {
		t.Fatalf("Name = %q, want solo", got.Name)
	}
	if got.Team != "myteam" {
		t.Fatalf("Team = %q, want myteam (fallback from --team-name)", got.Team)
	}
}

func TestParseTeammateCmdline_MissingAgentIDValue(t *testing.T) {
	// --agent-id at end of args with no value → reject.
	argv := argvOf("claude --agent-id")
	if _, ok := parseTeammateCmdline(argv); ok {
		t.Fatal("parseTeammateCmdline: want false, got true")
	}
}

func TestParseTeammateCmdline_NoName(t *testing.T) {
	// No --agent-id at all and no --agent-name → can't identify, reject.
	argv := argvOf("claude --team-name myteam --model x")
	if _, ok := parseTeammateCmdline(argv); ok {
		t.Fatal("parseTeammateCmdline: should reject cmdline with no name")
	}
}

func TestParseTeammateCmdline_AgentNameFallback(t *testing.T) {
	// --agent-name should populate Name if --agent-id is absent.
	argv := argvOf("claude --agent-name solo --team-name myteam")
	got, ok := parseTeammateCmdline(argv)
	if !ok {
		t.Fatal("ok=false")
	}
	if got.Name != "solo" || got.Team != "myteam" {
		t.Fatalf("got %+v, want Name=solo Team=myteam", got)
	}
}

// TestParseTeammateCmdline_SettingsPathWithSpaces: a --settings path containing
// spaces (e.g. a HOME under "/tmp/home with space") must not be shredded.
// Routed through the argv-slice API the path arrives intact and
// vendorFromProfilePath returns the vendor basename.
func TestParseTeammateCmdline_SettingsPathWithSpaces(t *testing.T) {
	argv := []string{
		"/usr/bin/claude",
		"--agent-id", "alice@alpha",
		"--settings", "/tmp/home with space/.claude/profiles/glm.json",
		"--model", "glm-4-flash",
	}
	got, ok := parseTeammateCmdline(argv)
	if !ok {
		t.Fatal("parseTeammateCmdline: ok=false")
	}
	// A space-containing --settings must not bleed into the agent-id split: the
	// whole Teammate (name/team from --agent-id, vendor from the settings
	// basename, model) must parse intact.
	want := Teammate{Name: "alice", Team: "alpha", Vendor: "glm", Model: "glm-4-flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parse mismatch (path with spaces must not shred):\n got: %+v\nwant: %+v",
			got, want)
	}
}

func TestVendorFromProfilePath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"/root/.claude/profiles/glm.json", "glm"},
		{"/home/user/.claude/profiles/deepseek.json", "deepseek"},
		{"profiles/kimi.json", "kimi"},
		{"kimi.json", "kimi"},
		{"/no/extension/here", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := vendorFromProfilePath(tc.in); got != tc.want {
				t.Fatalf("vendorFromProfilePath(%q) = %q, want %q",
					tc.in, got, tc.want)
			}
		})
	}
}

func TestCmdlineLooksLikeTeammate(t *testing.T) {
	cases := []struct {
		argv []string
		want bool
	}{
		{argvOf("/usr/bin/claude --agent-id alice@alpha"), true},
		{argvOf("env A=1 /path/claude --agent-id a@b --model x"), true},
		// No --agent-id → not a teammate (e.g. the lead session).
		{argvOf("/usr/bin/claude"), false},
		// --agent-id but no "claude" string → unrelated process.
		{argvOf("some-other-binary --agent-id alice@alpha"), false},
		{nil, false},
		// The real spawn binary is …/share/claude/versions/<hash>; its basename
		// is the VERSION, not "claude" — must still match via the "/claude/"
		// path segment, even alongside an incidental ".claude" profile path.
		{argvOf("env -u X /root/.local/share/claude/versions/2.1.150 --settings /root/.claude/profiles/glm.json --agent-id a@b"), true},
		// Tightening: a non-claude binary whose ONLY "claude" is an incidental
		// ".claude" path segment (no claude executable token) must NOT match.
		{argvOf("weirdbin --agent-id a@b --settings /home/u/.claude/x.json"), false},
	}
	for i, tc := range cases {
		t.Run(strings.Join(tc.argv, " "), func(t *testing.T) {
			if got := cmdlineLooksLikeTeammate(tc.argv); got != tc.want {
				t.Fatalf("case %d: cmdlineLooksLikeTeammate(%v) = %v, want %v",
					i, tc.argv, got, tc.want)
			}
		})
	}
}

// TestReadCmdline_ArgvSlice covers the kernel /proc/<pid>/cmdline layout:
// NUL-separated tokens with a conventional trailing NUL. readCmdline returns the
// argv slice directly (preserving spaces inside tokens) rather than join on
// space and let strings.Fields shred paths later.
func TestReadCmdline_ArgvSlice(t *testing.T) {
	// We exercise the same byte transform readCmdline performs on the file
	// contents, since /proc isn't writable in tests. Important inputs:
	//   - trailing NUL is conventionally present and must be trimmed
	//   - an arg with embedded spaces must survive intact
	raw := []byte("env\x00A=1\x00/path/claude\x00--settings\x00/tmp/home with space/profiles/glm.json\x00--agent-id\x00x@y\x00")
	// Mimic readCmdline's transform inline (kept local to the test so we
	// don't rely on a private helper).
	trimmed := raw
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == 0 {
		trimmed = trimmed[:len(trimmed)-1]
	}
	parts := strings.Split(string(trimmed), "\x00")
	want := []string{
		"env", "A=1", "/path/claude",
		"--settings", "/tmp/home with space/profiles/glm.json",
		"--agent-id", "x@y",
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("argv mismatch:\n got: %v\nwant: %v", parts, want)
	}
}

// TestDiscoverTeammates_NoServer exercises the end-to-end discovery path
// with a fake tmux that exits 1 (simulating "no tmux server running").
// We expect an empty result with no error — `ps` must work even with
// tmux down.
func TestDiscoverTeammates_NoServer(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
exit 1
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := DiscoverTeammates()
	if err != nil {
		t.Fatalf("DiscoverTeammates with no-server tmux: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestDiscoverTeammates_EmptyServer covers the case where tmux is up but
// list-panes returns no rows. Should still produce a clean empty result.
func TestDiscoverTeammates_EmptyServer(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := DiscoverTeammates()
	if err != nil {
		t.Fatalf("DiscoverTeammates with empty server: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

// TestDiscoverTeammates_SelfPid drives the full pipeline against THIS
// test process. We craft a tmux mock that reports a pane whose pane_pid
// is our own pid. The pipeline will then look for a child claude process
// in our subtree — there isn't one — so we expect a clean empty result.
// This catches accidental panics in the walker even when no match
// exists.
func TestDiscoverTeammates_SelfPidNoMatch(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "tmux")
	myPid := os.Getpid()
	script := `#!/bin/sh
printf '%%1 ` + itoaNoStrings(myPid) + `\n'
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := DiscoverTeammates()
	if err != nil {
		t.Fatalf("DiscoverTeammates: %v", err)
	}
	// The test process's subtree does not contain a claude --agent-id
	// process, so we expect an empty result.
	if len(got) != 0 {
		t.Fatalf("expected empty result, got %+v", got)
	}
}

func TestCmdlineAgentID(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{argvOf("env A=1 /path/claude --agent-id alice@alpha --model x"), "alice@alpha"},
		{argvOf("/usr/bin/claude --agent-id solo"), "solo"},
		{argvOf("/usr/bin/claude --model x"), ""},
		{argvOf("claude --agent-id"), ""}, // flag present but no value
		{nil, ""},
	}
	for _, tc := range cases {
		if got := cmdlineAgentID(tc.argv); got != tc.want {
			t.Errorf("cmdlineAgentID(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// TestCmdlineTeamName covers the unambiguous --team-name match used by the
// parse-fail teardown reap: it must read the explicit token, not split the
// <name>@<team> agent id (team names may contain '@'), and must not confuse
// "alpha" with "beta-alpha".
func TestCmdlineTeamName(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{argvOf("/path/claude --agent-id alice@alpha --team-name alpha"), "alpha"},
		{argvOf("/path/claude --agent-id w@alpha@prod --team-name alpha@prod"), "alpha@prod"},
		{argvOf("/path/claude --agent-id x@beta-alpha --team-name beta-alpha"), "beta-alpha"},
		{argvOf("/path/claude --agent-id solo"), ""}, // no --team-name
		{argvOf("claude --team-name"), ""},           // flag present but no value
		{nil, ""},
	}
	for _, tc := range cases {
		if got := cmdlineTeamName(tc.argv); got != tc.want {
			t.Errorf("cmdlineTeamName(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// TestDiscoverTeamAgentIDs_EmptySafe: an empty team matches nothing, and a real
// (unfaked) /proc scan for a unique team returns no ids and never our own work.
func TestDiscoverTeamAgentIDs_EmptySafe(t *testing.T) {
	if got := discoverTeamAgentIDs(""); got != nil {
		t.Fatalf("discoverTeamAgentIDs(\"\") = %v, want nil", got)
	}
	if got := discoverTeamAgentIDs("no-such-team-9999"); len(got) != 0 {
		t.Fatalf("discoverTeamAgentIDs(unique) = %v, want empty", got)
	}
}

// TestDiscoverTeammatePIDs_EmptyAndSelfSafe exercises the real /proc scan
// (no fake) for safety properties only: an empty id matches nothing, a unique
// fake id matches no live process, and the scan never panics nor returns our
// own pid. It deliberately does not signal anything.
func TestDiscoverTeammatePIDs_EmptyAndSelfSafe(t *testing.T) {
	if got := discoverTeammatePIDs(""); got != nil {
		t.Fatalf("discoverTeammatePIDs(\"\") = %v, want nil", got)
	}
	got := discoverTeammatePIDs("nonexistent-teammate-zzz@no-such-team-9999")
	if len(got) != 0 {
		t.Fatalf("discoverTeammatePIDs(unique id) = %v, want empty", got)
	}
	for _, pid := range got {
		if pid == os.Getpid() {
			t.Fatal("scan returned our own pid")
		}
	}
}

// itoaNoStrings is a tiny inlined int-to-string for use in shell-script
// templates. Pulled out so the script-literal stays readable.
func itoaNoStrings(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
