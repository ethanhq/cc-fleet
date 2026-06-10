package spawn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// captureStderr redirects os.Stderr for the duration of fn and returns whatever
// was written to it. Used to assert the probe's 5xx warning actually reaches
// stderr. Tests in this package run sequentially (none call t.Parallel), so
// swapping the process-global os.Stderr here is safe.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	fn()

	// Close the writer before reading so io.Copy sees EOF. The warning is a
	// single short line, well under the pipe buffer, so this can't deadlock.
	_ = w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	_ = r.Close()
	return buf.String()
}

// fixture is a per-test sandbox: isolated HOME / XDG, fake tmux on PATH,
// optional provider httptest.Server, optional fingerprint cache. Tests build
// one with newFixture(t) and then customize via mutator methods before
// calling Spawn.
type fixture struct {
	t        *testing.T
	home     string
	xdg      string
	argsPath string // captured fake-tmux argv log
	server   *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	// Install fake tmux on PATH. Uses the same scheme as
	// internal/tmux/tmux_test.go::mockTmux so we don't depend on it.
	binDir := t.TempDir()
	argsPath := filepath.Join(binDir, "args.log")
	binPath := filepath.Join(binDir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"
done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
# Out-of-tmux swarm calls are socket-scoped ("-L <socket> <subcommand> ...").
# Skip a leading -L so we can dispatch on the subcommand; the full argv (incl.
# -L) was already recorded above. In-tmux tests never pass -L, so this is inert
# for them. has-session honors $MOCK_HASSESSION_EXIT (default 0 = session
# "exists" → SpawnSwarm takes its split-window/additional-teammate branch, i.e.
# createdServer=false). A test wanting the first-teammate path (createdServer
# =true) sets MOCK_HASSESSION_EXIT=1 so the session is reported absent and
# SpawnSwarm takes its new-session branch.
if [ "$1" = "-L" ]; then shift 2; fi
case "$1" in
  ls)
    # PickAttachedSession: return one attached session named "0".
    printf '0 1 200\n'
    exit 0
    ;;
  has-session)
    exit ${MOCK_HASSESSION_EXIT:-0}
    ;;
  new-session)
    # SpawnSwarm first-teammate branch: return a deterministic pane id.
    printf '%%99\n'
    exit 0
    ;;
  split-window)
    # SplitWindow: return a deterministic pane id.
    printf '%%99\n'
    exit 0
    ;;
  display-message)
    # SessionForPane: reverse-look-up a pane's session name.
    printf 'caller-sess\n'
    exit 0
    ;;
  list-panes)
    exit 0
    ;;
  kill-pane)
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	// Neutralize the developer's ambient $TMUX_PANE so the default target path
	// is deterministically PickAttachedSession. Tests that exercise the
	// $TMUX_PANE branch opt in explicitly via t.Setenv.
	t.Setenv("TMUX_PANE", "")
	// Pin $TMUX non-empty so the default fixture is deterministically "inside
	// tmux": the spawn gate (useSwarm) keys on $TMUX, so without this the in-tmux
	// tests would flip to the out-of-tmux swarm branch whenever `go test` runs
	// outside tmux. Out-of-tmux tests opt in via t.Setenv("TMUX", "").
	t.Setenv("TMUX", "/tmp/tmux-test/default,1,0")

	return &fixture{t: t, home: home, xdg: xdg, argsPath: argsPath}
}

// writeProvidersTOML installs a providers.toml at the canonical XDG path. The
// caller can pass extra provider stanzas; the first one is always "deepseek"
// pointing at f.serverURL() (if a server is running) or example.invalid.
func (f *fixture) writeProvidersTOML(extra string) {
	f.t.Helper()
	dir := filepath.Join(f.xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatalf("mkdir config: %v", err)
	}
	modelsURL := "https://api.example.invalid/v1/models"
	baseURL := "https://api.example.invalid/anthropic"
	if f.server != nil {
		modelsURL = f.server.URL + "/v1/models"
		baseURL = f.server.URL + "/anthropic"
	}
	body := fmt.Sprintf(`version = 1

[deepseek]
base_url        = "%s"
default_model   = "deepseek-v4-flash"
models_endpoint = "%s"
secret_backend  = "file"
secret_ref      = "deepseek.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
%s
`, baseURL, modelsURL, extra)
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(body), 0o600); err != nil {
		f.t.Fatalf("write providers.toml: %v", err)
	}

	// Mirror production: `cc-fleet add` always persists the provider key, so the
	// (now key-bearing) spawn probe finds one on disk via secrets.Keyget. The
	// fake provider server ignores it; the probe's error classification is what
	// the spawn-probe tests assert.
	secretsDir, err := config.SecretsDir()
	if err != nil {
		f.t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		f.t.Fatalf("mkdir secrets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "deepseek.key"), []byte(probeKey), 0o600); err != nil {
		f.t.Fatalf("write secret: %v", err)
	}
}

// probeKey is the fake provider key the fixture persists; spawn-probe tests
// assert it is sent as the probe's bearer token. It deliberately avoids the
// "sk-" prefix the happy-path test treats as a key-leak canary.
const probeKey = "probe-test-key-deadbeef"

// writeFingerprint installs a usable fingerprint.json at the canonical path.
//
// spawn runs fingerprint.ValidateForRuntime BEFORE any state mutation, which
// os.Stats fp.BinaryPath, so we write a real fake binary in t.TempDir() to pass
// that gate while still exercising the buildSpawnCommand argv shape (which only
// reads the path string, never execs it).
func (f *fixture) writeFingerprint() {
	f.t.Helper()
	// Drop a placeholder binary so ValidateForRuntime's os.Stat succeeds.
	binDir := f.t.TempDir()
	binPath := filepath.Join(binDir, "claude-2.1.150")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		f.t.Fatalf("write fake claude binary: %v", err)
	}
	fp := &fingerprint.Fingerprint{
		CCVersion:  "2.1.150",
		CapturedAt: time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC),
		BinaryPath: binPath,
		Env: map[string]string{
			"CLAUDECODE":                           "1",
			"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
		},
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
			"--agent-name", "{name}",
			"--team-name", "{team}",
			"--agent-color", "{color}",
			"--parent-session-id", "{lead_session_id}",
			"--agent-type", "general-purpose",
			"--dangerously-skip-permissions",
		},
	}
	if err := fingerprint.Save(fp); err != nil {
		f.t.Fatalf("Save fingerprint: %v", err)
	}
}

// installFakeClaude puts an executable `claude` on PATH so ccver.Detect (and
// thus fingerprint.ResolveBinaryPath) resolves a binary deterministically in
// tests that exercise the bundled-recipe / dynamic-path flow, independent of
// whether the host has a real Claude Code install. It answers --version so
// ccver can read a version too.
func (f *fixture) installFakeClaude() string {
	f.t.Helper()
	dir := f.t.TempDir()
	bin := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo \"2.1.150 (Claude Code)\";; esac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		f.t.Fatalf("write fake claude: %v", err)
	}
	f.t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// startProviderServer brings up an httptest.Server that responds 200 OK to
// any GET. The fixture wires writeProvidersTOML to point at it.
func (f *fixture) startProviderServer() {
	f.t.Helper()
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	f.t.Cleanup(f.server.Close)
}

// readMockArgs returns the args.log split into per-invocation argv slices.
func (f *fixture) readMockArgs() [][]string {
	f.t.Helper()
	data, err := os.ReadFile(f.argsPath)
	if err != nil {
		// No tmux calls happened — return empty.
		if os.IsNotExist(err) {
			return nil
		}
		f.t.Fatalf("read args.log: %v", err)
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "__END__" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		if line == "" {
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

// splitWindowCall returns the recorded `tmux split-window ...` invocation, or
// fails the test if none was recorded.
func (f *fixture) splitWindowCall() []string {
	f.t.Helper()
	for _, c := range f.readMockArgs() {
		if len(c) > 0 && c[0] == "split-window" {
			return c
		}
	}
	f.t.Fatalf("no split-window call recorded; calls=%v", f.readMockArgs())
	return nil
}

// ----- happy paths -----

func TestSpawn_HappyPath(t *testing.T) {
	f := newFixture(t)
	f.startProviderServer()
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-1",
		Team:      "myproj",
		Probe:     true,
		AutoTeam:  true,
	})

	if !res.OK {
		t.Fatalf("Spawn: ok=false; code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.AgentID != "worker-1@myproj" {
		t.Fatalf("AgentID = %q, want worker-1@myproj", res.AgentID)
	}
	if res.PaneID != "%99" {
		t.Fatalf("PaneID = %q, want %%99 (from fake tmux)", res.PaneID)
	}
	if res.Model != "deepseek-v4-flash" {
		t.Fatalf("Model = %q, want default", res.Model)
	}
	if res.TmuxSession != "0" {
		t.Fatalf("TmuxSession = %q, want 0 (from PickAttachedSession)", res.TmuxSession)
	}
	if res.Color == "" {
		t.Fatal("Color should be auto-picked, got empty")
	}

	// Verify tmux split-window was actually called.
	calls := f.readMockArgs()
	var splitCall []string
	for _, c := range calls {
		if len(c) > 0 && c[0] == "split-window" {
			splitCall = c
			break
		}
	}
	if splitCall == nil {
		t.Fatalf("no split-window call recorded; calls=%v", calls)
	}
	// The last arg of split-window is the shell command. It must contain
	// the binary path, --settings <profile>, --model, and apiKeyHelper
	// must NOT leak the actual key (just a profile path reference).
	cmd := splitCall[len(splitCall)-1]
	// Re-read the fingerprint we just wrote so we assert against the per-test
	// temp path the fixture generated — fingerprint paths are real on-disk files
	// in t.TempDir(), so there's no stable hard-coded path to assert against.
	fp, fpErr := fingerprint.Load()
	if fpErr != nil {
		t.Fatalf("fingerprint.Load: %v", fpErr)
	}
	for _, want := range []string{
		fp.BinaryPath,
		"--agent-id",
		"'worker-1@myproj'",
		"--settings",
		"--model",
		"'deepseek-v4-flash'",
		"CLAUDECODE=1",
		"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("split-window cmd missing %q\n  cmd: %s", want, cmd)
		}
	}
	// Hardening: the command must explicitly unset the main session's
	// Anthropic credentials so a provider teammate can't inherit them from the
	// tmux server environment (e.g. an ANTHROPIC_API_KEY-mode main session).
	for _, want := range []string{"-u ANTHROPIC_API_KEY", "-u ANTHROPIC_AUTH_TOKEN"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("split-window cmd missing credential unset %q\n  cmd: %s", want, cmd)
		}
	}
	// Critical: NO actual key VALUE may appear in the command line. (The var
	// NAMES are allowed only as `env -u` arguments, asserted above.) The `sk-`
	// check is a generic canary; the load-bearing assertion is that the ACTUAL
	// seeded on-disk key never reaches argv — it must resolve only via
	// apiKeyHelper/keyget at runtime.
	if strings.Contains(cmd, "sk-") {
		t.Fatalf("split-window cmd leaked a key value:\n  %s", cmd)
	}
	if strings.Contains(cmd, probeKey) {
		t.Fatalf("split-window cmd leaked the seeded key %q:\n  %s", probeKey, cmd)
	}

	// Verify the team config was created and contains the member.
	tc, err := LoadTeamConfig("myproj")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if len(tc.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(tc.Members))
	}
	if tc.Members[0].Name != "worker-1" {
		t.Fatalf("member name = %q, want worker-1", tc.Members[0].Name)
	}
	if tc.Members[0].TmuxPaneID != "%99" {
		t.Fatalf("member paneID = %q, want %%99", tc.Members[0].TmuxPaneID)
	}
	// The main session renders the teammate from these fields; cc-fleet must
	// write them or the teammate shows up colorless / statusless (the bug).
	if tc.Members[0].Color == "" {
		t.Fatal("member Color should be persisted (auto-picked), got empty")
	}
	if tc.Members[0].BackendType != "tmux" {
		t.Fatalf("member BackendType = %q, want tmux", tc.Members[0].BackendType)
	}
	if !tc.Members[0].IsActive {
		t.Fatal("member IsActive should be true after spawn")
	}
	if tc.LeadSessionID == "" {
		t.Fatal("LeadSessionID should be set after AutoTeam spawn")
	}

	// Verify inbox file was pre-created.
	inboxPath, _ := InboxPath("myproj", "worker-1")
	if _, err := os.Stat(inboxPath); err != nil {
		t.Fatalf("inbox not created: %v", err)
	}
}

func TestSpawn_NoProbe(t *testing.T) {
	// Same as happy path but skip the provider probe — uses example.invalid
	// which would fail DNS if we tried to reach it.
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "worker-2",
		Team:      "p2",
		Probe:     false,
		AutoTeam:  true,
	})
	if !res.OK {
		t.Fatalf("Spawn no-probe: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
}

func TestSpawn_ModelOverride(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "w",
		Team:      "t",
		Model:     "deepseek-reasoner-custom",
		Probe:     false,
		AutoTeam:  true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.Model != "deepseek-reasoner-custom" {
		t.Fatalf("Model = %q, want override value", res.Model)
	}
}

func TestSpawn_ExplicitColor(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "w",
		Team:      "t",
		Color:     "purple-unicorn",
		Probe:     false,
		AutoTeam:  true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.Color != "purple-unicorn" {
		t.Fatalf("Color = %q, want explicit override", res.Color)
	}
}

func TestSpawn_LeadSessionIDOverride(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	lead := "00000000-1111-2222-3333-444444444444"
	res := Spawn(Request{
		Provider:      "deepseek",
		AgentName:     "w",
		Team:          "t",
		LeadSessionID: lead,
		Probe:         false,
		AutoTeam:      true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	tc, err := LoadTeamConfig("t")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if tc.LeadSessionID != lead {
		t.Fatalf("LeadSessionID = %q, want %q", tc.LeadSessionID, lead)
	}
}

// TestSpawn_PrefersTmuxPaneEnv verifies that when no --target is given but
// $TMUX_PANE is set, spawn splits off that exact pane (so the teammate lands
// beside the caller) and reports the pane's resolved session name.
func TestSpawn_PrefersTmuxPaneEnv(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX_PANE", "%63")

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t",
		Probe: false, AutoTeam: true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}

	split := f.splitWindowCall()
	if split[1] != "-t" || split[2] != "%63" {
		t.Fatalf("split-window target = %q %q, want -t %%63 (from $TMUX_PANE)", split[1], split[2])
	}
	// TmuxSession is the reverse-looked-up session name (fake tmux's
	// display-message returns "caller-sess").
	if res.TmuxSession != "caller-sess" {
		t.Fatalf("TmuxSession = %q, want caller-sess (resolved from pane)", res.TmuxSession)
	}
}

// TestSpawn_ExplicitTargetBeatsEnv verifies --target outranks $TMUX_PANE.
func TestSpawn_ExplicitTargetBeatsEnv(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX_PANE", "%63")

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t",
		Target: "explicit-sess",
		Probe:  false, AutoTeam: true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}

	split := f.splitWindowCall()
	if split[2] != "explicit-sess" {
		t.Fatalf("split-window target = %q, want explicit-sess (--target outranks $TMUX_PANE)", split[2])
	}
	if res.TmuxSession != "explicit-sess" {
		t.Fatalf("TmuxSession = %q, want explicit-sess", res.TmuxSession)
	}
}

// ----- out-of-tmux swarm gate -----

// TestUseSwarm is the gate predicate's truth table: swarm only when there's no
// explicit --target AND we're not inside tmux ($TMUX empty). An explicit target
// always keeps the in-tmux path, even outside tmux.
func TestUseSwarm(t *testing.T) {
	cases := []struct {
		target, tmuxEnv string
		want            bool
	}{
		{"", "", true},                                // no target + outside tmux → swarm
		{"", "/tmp/tmux-0/default,1,0", false},        // no target but inside tmux → in-tmux
		{"%5", "", false},                             // explicit target outside tmux → in-tmux
		{"explicit-sess", "/tmp/tmux-0/d,1,0", false}, // explicit target inside tmux → in-tmux
	}
	for _, tc := range cases {
		if got := useSwarm(tc.target, tc.tmuxEnv); got != tc.want {
			t.Errorf("useSwarm(%q,%q) = %v, want %v", tc.target, tc.tmuxEnv, got, tc.want)
		}
	}
}

// TestSpawn_OutOfTmuxUsesSwarm: with $TMUX empty and no --target, Spawn takes the
// swarm branch (private socket cc-fleet-swarm-<team>) and short-circuits BEFORE
// PickAttachedSession — even though an unrelated attached server is available
// (the fake tmux's `ls` would return one). The persisted config records the
// socket so a later teardown/ps can find it.
func TestSpawn_OutOfTmuxUsesSwarm(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX", "")      // not inside tmux
	t.Setenv("TMUX_PANE", "") // (already neutralized; explicit for intent)

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w1", Team: "swarmt",
		Probe: false, AutoTeam: true,
	})
	if !res.OK {
		t.Fatalf("swarm spawn ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.TmuxSocket != "cc-fleet-swarm-swarmt" {
		t.Fatalf("TmuxSocket = %q, want cc-fleet-swarm-swarmt (swarm branch taken)", res.TmuxSocket)
	}
	if res.TmuxSession != "claude-swarm" {
		t.Fatalf("TmuxSession = %q, want claude-swarm", res.TmuxSession)
	}
	if res.AttachCommand != "tmux -L cc-fleet-swarm-swarmt attach -t claude-swarm" {
		t.Fatalf("AttachCommand = %q", res.AttachCommand)
	}
	// Gate proof: PickAttachedSession (`tmux ls`) must NOT have been called, even
	// though the fake server has an attached session ready to be picked.
	for _, c := range f.readMockArgs() {
		if len(c) > 0 && c[0] == "ls" {
			t.Fatalf("swarm gate must short-circuit before PickAttachedSession; saw ls: %v", f.readMockArgs())
		}
	}
	// Config persisted the socket marker (Raw round-trip).
	tc, err := LoadTeamConfig("swarmt")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if tc.TmuxSocket() != "cc-fleet-swarm-swarmt" {
		t.Fatalf("persisted socket = %q, want cc-fleet-swarm-swarmt", tc.TmuxSocket())
	}
}

// TestSpawn_ExplicitTargetStaysInTmuxOutsideTmux: an explicit --target keeps the
// in-tmux split path even when $TMUX is empty (the user named the target), and
// the result carries NO swarm socket.
func TestSpawn_ExplicitTargetStaysInTmuxOutsideTmux(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()
	t.Setenv("TMUX", "") // outside tmux, but --target is explicit

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t",
		Target: "explicit-sess",
		Probe:  false, AutoTeam: true,
	})
	if !res.OK {
		t.Fatalf("Spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if res.TmuxSocket != "" {
		t.Fatalf("explicit --target must stay in-tmux, got TmuxSocket=%q", res.TmuxSocket)
	}
	// In-tmux split-window into the named target, not a swarm session.
	split := f.splitWindowCall()
	if split[2] != "explicit-sess" {
		t.Fatalf("split-window target = %q, want explicit-sess", split[2])
	}
}

// ----- error paths -----

func TestSpawn_UnknownProvider(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider:  "nonesuch",
		AgentName: "w",
		Team:      "t",
		Probe:     false,
		AutoTeam:  true,
	})
	if res.OK {
		t.Fatal("Spawn: want failure for unknown provider")
	}
	if res.ErrorCode != ErrCodeUnknownProvider {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrCodeUnknownProvider)
	}
}

func TestSpawn_ProviderDisabled(t *testing.T) {
	f := newFixture(t)
	f.writeFingerprint()
	dir := filepath.Join(f.xdg, "cc-fleet")
	_ = os.MkdirAll(dir, 0o700)
	body := `version = 1

[deepseek]
base_url        = "https://api.example.invalid/anthropic"
default_model   = "deepseek-v4-flash"
models_endpoint = "https://api.example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "deepseek.key"
enabled         = false
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res := Spawn(Request{Provider: "deepseek", AgentName: "w", Team: "t", Probe: false, AutoTeam: true})
	if res.OK || res.ErrorCode != ErrCodeProviderDisabled {
		t.Fatalf("got OK=%v code=%q, want disabled failure", res.OK, res.ErrorCode)
	}
}

// TestSpawn_NoUserFingerprint_UsesBundled: a fresh install with NO
// ~/.config/cc-fleet/fingerprint.json must NOT fail with FINGERPRINT_MISSING.
// LoadOrBundled supplies the embedded recipe and the binary path is resolved
// live (a fake claude on PATH here), so the spawn goes through.
func TestSpawn_NoUserFingerprint_UsesBundled(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.installFakeClaude() // so ResolveBinaryPath finds a binary deterministically
	// Deliberately do NOT writeFingerprint → LoadOrBundled returns the bundled recipe.

	res := Spawn(Request{
		Provider:  "deepseek",
		AgentName: "w",
		Team:      "t",
		Probe:     false,
		AutoTeam:  true,
	})
	if res.ErrorCode == ErrCodeFingerprintMissing {
		t.Fatalf("missing user fingerprint must fall back to the bundled recipe, got FINGERPRINT_MISSING: %s", res.ErrorMsg)
	}
	if !res.OK {
		t.Fatalf("spawn with bundled recipe should succeed (fake tmux+claude), got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
	// The bundled recipe's placeholders must be SUBSTITUTED with the real
	// name/team before reaching the split command — not pass through literally.
	joined := strings.Join(f.splitWindowCall(), " ")
	for _, want := range []string{"--agent-name", "--team-name"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("bundled recipe flag %q missing from spawn command: %s", want, joined)
		}
	}
	if strings.Contains(joined, "{name}") || strings.Contains(joined, "{team}") {
		t.Fatalf("bundled recipe placeholders left unsubstituted: %s", joined)
	}
	// agent-id carries an '@' so it is quoted; this proves {name}@{team} resolved.
	if !strings.Contains(joined, "'w@t'") {
		t.Fatalf("substituted agent-id 'w@t' missing from spawn command: %s", joined)
	}
}

// TestSpawn_HTTP500_DoesNotBlockSpawn: an HTTP 500 from the probe means the
// provider RESPONDED, so the network is reachable. The models endpoint is only
// used for probe/refresh — the teammate authenticates against base_url at
// runtime — so a 5xx must NOT block the spawn.
func TestSpawn_HTTP500_DoesNotBlockSpawn(t *testing.T) {
	f := newFixture(t)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(f.server.Close)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	var res Result
	stderr := captureStderr(t, func() {
		res = Spawn(Request{
			Provider: "deepseek", AgentName: "w", Team: "t", Probe: true, AutoTeam: true,
		})
	})
	if !res.OK {
		t.Fatalf("got OK=false code=%q msg=%q, want spawn to proceed past a 5xx probe",
			res.ErrorCode, res.ErrorMsg)
	}
	// Proceeding silently would hide a genuinely sick provider, so the 5xx path
	// must still emit a warning. Assert it actually reached stderr.
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "HTTP 500") {
		t.Fatalf("expected a 5xx probe warning on stderr, got: %q", stderr)
	}
}

// TestSpawn_HTTP401_KeyInvalid: a 401 from the probe is an AUTH failure (key
// rejected), not unreachability, so it maps to KEY_INVALID. It also asserts the
// probe SENDS the provider key as a bearer token — a keyless probe would 401 on
// every key-gated provider and get mislabeled PROVIDER_UNREACHABLE.
func TestSpawn_HTTP401_KeyInvalid(t *testing.T) {
	f := newFixture(t)
	var sawAuth string
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(f.server.Close)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t", Probe: true, AutoTeam: true,
	})
	if res.OK || res.ErrorCode != ErrCodeKeyInvalid {
		t.Fatalf("got OK=%v code=%q, want KEY_INVALID", res.OK, res.ErrorCode)
	}
	if want := "Bearer " + probeKey; sawAuth != want {
		t.Fatalf("probe Authorization = %q, want %q (probe must send the key)", sawAuth, want)
	}
}

// TestSpawn_HTTP403_KeyInvalid verifies 403 is also classified as an auth
// failure (key forbidden / insufficient permissions), not unreachability.
func TestSpawn_HTTP403_KeyInvalid(t *testing.T) {
	f := newFixture(t)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(f.server.Close)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t", Probe: true, AutoTeam: true,
	})
	if res.OK || res.ErrorCode != ErrCodeKeyInvalid {
		t.Fatalf("got OK=%v code=%q, want KEY_INVALID", res.OK, res.ErrorCode)
	}
}

func TestSpawn_ProviderUnreachable_Timeout(t *testing.T) {
	f := newFixture(t)
	// Server hangs forever — probe must time out at 3s and surface
	// PROVIDER_UNREACHABLE rather than hanging the spawn.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)
	f.server = hang
	f.writeProvidersTOML("")
	f.writeFingerprint()

	start := time.Now()
	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t", Probe: true, AutoTeam: true,
	})
	elapsed := time.Since(start)

	if res.OK || res.ErrorCode != ErrCodeProviderUnreachable {
		t.Fatalf("got OK=%v code=%q, want PROVIDER_UNREACHABLE", res.OK, res.ErrorCode)
	}
	// Probe is 3s; total elapsed should be in the 3-5s window. Anything
	// >>5s means the timeout didn't fire.
	if elapsed > 5*time.Second {
		t.Fatalf("Spawn took %v with hanging provider, want < 5s (probe timeout broken)", elapsed)
	}
}

// TestSpawn_ProviderUnreachable_ConnRefused verifies the other half of the
// transport-failure classification: a refused TCP dial (no HTTP response at
// all) is a connection-layer failure → PROVIDER_UNREACHABLE, distinct from the
// timeout case above and from any HTTP status. models_endpoint points at a
// closed local port (127.0.0.1:1, the reserved tcpmux port that is never
// listening in a test env), so the dial is refused immediately — no waiting,
// no network egress. The closed-loop counterpart to the HTTP-status cases:
// transport-down really does block, while any HTTP answer (4xx/5xx) does not.
func TestSpawn_ProviderUnreachable_ConnRefused(t *testing.T) {
	f := newFixture(t)
	f.writeFingerprint()
	dir := filepath.Join(f.xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	body := `version = 1

[deepseek]
base_url        = "http://127.0.0.1:1/anthropic"
default_model   = "deepseek-v4-flash"
models_endpoint = "http://127.0.0.1:1/v1/models"
secret_backend  = "file"
secret_ref      = "deepseek.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write providers.toml: %v", err)
	}

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t", Probe: true, AutoTeam: true,
	})
	if res.OK || res.ErrorCode != ErrCodeProviderUnreachable {
		t.Fatalf("got OK=%v code=%q, want PROVIDER_UNREACHABLE (connection refused)",
			res.OK, res.ErrorCode)
	}
}

func TestSpawn_TeamNotFoundWithoutAutoTeam(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "missing", Probe: false, AutoTeam: false,
	})
	if res.OK || res.ErrorCode != ErrCodeTeamNotFound {
		t.Fatalf("got OK=%v code=%q, want TEAM_NOT_FOUND", res.OK, res.ErrorCode)
	}
}

// TestSpawn_FlockSerializesDuplicate verifies two concurrent spawns with the
// SAME (team, name) end with exactly one successful spawn and one explicit
// DUPLICATE_NAME failure — and crucially that tmux SplitWindow was invoked only
// ONCE, so the losing spawn never leaks a pane. Counting Members alone would
// miss the leak.
func TestSpawn_FlockSerializesDuplicate(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	const team = "concurrent"
	const name = "twin"

	var wg sync.WaitGroup
	results := make([]Result, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = Spawn(Request{
				Provider: "deepseek", AgentName: name, Team: team,
				Probe: false, AutoTeam: true,
			})
		}()
	}
	wg.Wait()

	// Exactly one OK + exactly one DUPLICATE_NAME, regardless of scheduling.
	var okCount, dupCount int
	for _, r := range results {
		if r.OK {
			okCount++
		} else if r.ErrorCode == ErrCodeDuplicateName {
			dupCount++
		} else {
			t.Fatalf("unexpected failure: code=%s msg=%s", r.ErrorCode, r.ErrorMsg)
		}
	}
	if okCount != 1 || dupCount != 1 {
		t.Fatalf("got OK=%d DUPLICATE=%d, want 1+1", okCount, dupCount)
	}

	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if len(tc.Members) != 1 {
		t.Fatalf("members = %d (%+v), want 1", len(tc.Members), tc.Members)
	}

	// Load-bearing: tmux SplitWindow should have been called exactly ONCE — a
	// second split would be a silent pane leak.
	splitCount := 0
	for _, c := range f.readMockArgs() {
		if len(c) > 0 && c[0] == "split-window" {
			splitCount++
		}
	}
	if splitCount != 1 {
		t.Fatalf("tmux split-window calls = %d, want 1 (second spawn leaked a pane!)", splitCount)
	}
}

// TestSpawn_FlockSerializesDifferentNames verifies that two concurrent
// spawns into the same team with DIFFERENT names both register cleanly
// and the team config ends up with both members.
func TestSpawn_FlockSerializesDifferentNames(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	const team = "concurrent2"

	var wg sync.WaitGroup
	for _, name := range []string{"alpha", "beta"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := Spawn(Request{
				Provider: "deepseek", AgentName: name, Team: team,
				Probe: false, AutoTeam: true,
			})
			if !res.OK {
				t.Errorf("spawn %s: code=%s msg=%s", name, res.ErrorCode, res.ErrorMsg)
			}
		}()
	}
	wg.Wait()

	tc, err := LoadTeamConfig(team)
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if len(tc.Members) != 2 {
		t.Fatalf("members = %d, want 2; got %+v", len(tc.Members), tc.Members)
	}
}

// ----- low-level helpers -----

func TestBuildSpawnCommand_QuotesEverything(t *testing.T) {
	fp := &fingerprint.Fingerprint{
		BinaryPath: "/usr/bin/claude",
		Env:        map[string]string{"CLAUDECODE": "1"},
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
		},
	}
	ctx := fingerprint.SpawnContext{Name: "evil; rm -rf", Team: "t", Color: "c", LeadSessionID: "x"}
	cmd, err := buildSpawnCommand(fp, ctx, "/tmp/p.json", "model'with'quote", nil, false)
	if err != nil {
		t.Fatalf("buildSpawnCommand: %v", err)
	}
	// The dangerous chars must be inside single-quotes — verify the literal
	// post-quote string appears intact.
	if !strings.Contains(cmd, "'evil; rm -rf@t'") {
		t.Fatalf("agent id not properly quoted in: %s", cmd)
	}
	if !strings.Contains(cmd, `'model'\''with'\''quote'`) {
		t.Fatalf("model with embedded quote not properly escaped in: %s", cmd)
	}
}

// permFP builds a fingerprint whose flag template carries a frozen
// --dangerously-skip-permissions, the shape the permission-inheritance code
// must strip.
func permFP() *fingerprint.Fingerprint {
	return &fingerprint.Fingerprint{
		BinaryPath: "/usr/bin/claude",
		Env:        map[string]string{"CLAUDECODE": "1"},
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
			"--dangerously-skip-permissions",
			"--agent-type", "general-purpose",
		},
	}
}

// TestBuildSpawnCommand_PermissionInheritance_StripsFingerprintFlag: when source
// is not frozen-template, the fingerprint's captured
// --dangerously-skip-permissions is removed and replaced by the inherited
// decision — here lead-default (no flag), so the teammate carries no permission
// flag at all.
func TestBuildSpawnCommand_PermissionInheritance_StripsFingerprintFlag(t *testing.T) {
	ctx := fingerprint.SpawnContext{Name: "w", Team: "t", Color: "c", LeadSessionID: "x"}
	cmd, err := buildSpawnCommand(permFP(), ctx, "/tmp/p.json", "m", nil, true)
	if err != nil {
		t.Fatalf("buildSpawnCommand: %v", err)
	}
	if strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("frozen flag should have been stripped, got: %s", cmd)
	}
	// Non-permission flags survive the strip.
	if !strings.Contains(cmd, "--agent-type") || !strings.Contains(cmd, "general-purpose") {
		t.Fatalf("non-permission flags must survive strip, got: %s", cmd)
	}
}

// TestBuildSpawnCommand_PermissionInheritance_AppendsInherited: lead-flag
// acceptEdits strips the frozen bypass and appends the inherited
// --permission-mode acceptEdits.
func TestBuildSpawnCommand_PermissionInheritance_AppendsInherited(t *testing.T) {
	ctx := fingerprint.SpawnContext{Name: "w", Team: "t", Color: "c", LeadSessionID: "x"}
	cmd, err := buildSpawnCommand(permFP(), ctx, "/tmp/p.json", "m",
		[]string{"--permission-mode", "acceptEdits"}, true)
	if err != nil {
		t.Fatalf("buildSpawnCommand: %v", err)
	}
	if strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("frozen bypass should be stripped, got: %s", cmd)
	}
	// The inherited value must stay COUPLED to its --permission-mode flag — a
	// bare, detached "acceptEdits" token does not satisfy the contract. tmux.Quote
	// wraps each template token in single quotes, so the coupled pair appears as
	// '--permission-mode' 'acceptEdits' (accept an unquoted shape too in case the
	// quoting policy for simple tokens ever changes).
	if !strings.Contains(cmd, "'--permission-mode' 'acceptEdits'") &&
		!strings.Contains(cmd, "--permission-mode acceptEdits") {
		t.Fatalf("inherited --permission-mode acceptEdits flag missing or detached, got: %s", cmd)
	}
}

// TestBuildSpawnCommand_PermissionInheritance_StripsDangerousFlagOnFallback:
// production passes stripPerms=true UNCONDITIONALLY — including the
// frozen-template fallback (inherited=nil, the undetectable-lead / macOS /
// out-of-tmux case). So the fingerprint's captured --dangerously-skip-permissions
// must be STRIPPED, not silently inherited; the teammate falls back to claude's
// safe interactive default and an operator who wants bypass passes it explicitly
// (source=manual). This test FAILS if a production fallback command ever carries
// the bypass flag again.
func TestBuildSpawnCommand_PermissionInheritance_StripsDangerousFlagOnFallback(t *testing.T) {
	ctx := fingerprint.SpawnContext{Name: "w", Team: "t", Color: "c", LeadSessionID: "x"}
	// Mirror exactly how spawn.go builds the frozen-template fallback:
	// inherited=nil, stripPerms=true.
	cmd, err := buildSpawnCommand(permFP(), ctx, "/tmp/p.json", "m", nil, true)
	if err != nil {
		t.Fatalf("buildSpawnCommand: %v", err)
	}
	if strings.Contains(cmd, "--dangerously-skip-permissions") {
		t.Fatalf("frozen-template fallback must STRIP --dangerously-skip-permissions (R-macOS-008), got: %s", cmd)
	}
	// Non-permission flags from the template still survive the strip.
	if !strings.Contains(cmd, "--agent-type") || !strings.Contains(cmd, "general-purpose") {
		t.Fatalf("non-permission flags must survive strip, got: %s", cmd)
	}
}

// TestBuildSpawnCommand_FrozenTemplate_StripsThenByteStable: the production
// frozen-template path (inherited=nil, stripPerms=true) must produce argv
// byte-identical to a hand-built command whose ONLY change vs the raw template
// is the permission-flag strip — the captured --dangerously-skip-permissions
// removed, nothing else shifted.
func TestBuildSpawnCommand_FrozenTemplate_StripsThenByteStable(t *testing.T) {
	fp := permFP()
	ctx := fingerprint.SpawnContext{Name: "w", Team: "t", Color: "c", LeadSessionID: "x"}

	got, err := buildSpawnCommand(fp, ctx, "/tmp/p.json", "m", nil, true)
	if err != nil {
		t.Fatalf("buildSpawnCommand: %v", err)
	}

	// Hand-build the expectation: env prefix + binary + the applied template with
	// permission flags STRIPPED + --settings + --model.
	var want []string
	want = append(want, "env", "-u", "ANTHROPIC_API_KEY", "-u", "ANTHROPIC_AUTH_TOKEN")
	// The model/effort env is unset so the launching shell can't override the
	// profile (same key list childenv strips on the subagent/run path).
	for _, k := range childenv.ModelEnvKeys {
		want = append(want, "-u", k)
	}
	want = append(want, tmux.Quote("CLAUDECODE=1"))
	want = append(want, tmux.Quote("/usr/bin/claude"))
	for _, f := range stripPermissionFlags(fingerprint.Apply(fp, ctx)) {
		want = append(want, tmux.Quote(f))
	}
	want = append(want, "--settings", tmux.Quote("/tmp/p.json"), "--model", tmux.Quote("m"))
	if got != strings.Join(want, " ") {
		t.Fatalf("frozen-template argv drifted from the stripped expectation:\n got=%s\nwant=%s", got, strings.Join(want, " "))
	}
	// Belt-and-braces: the bypass flag must be absent from the production shape.
	if strings.Contains(got, "--dangerously-skip-permissions") {
		t.Fatalf("frozen-template path must not carry --dangerously-skip-permissions, got: %s", got)
	}
}

func TestEnsureMember_Idempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := Member{Name: "alice", AgentID: "alice@x", AgentType: "general-purpose"}
	if added, err := EnsureMember("x", m); err != nil || !added {
		t.Fatalf("first add: added=%v err=%v", added, err)
	}
	if added, err := EnsureMember("x", m); err != nil || added {
		t.Fatalf("second add: added=%v (want false) err=%v", added, err)
	}
	tc, err := LoadTeamConfig("x")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if len(tc.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(tc.Members))
	}
}

func TestEnsureInbox_PreservesExisting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := EnsureTeamDir("x"); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	// Seed an inbox with non-empty content; EnsureInbox must NOT overwrite.
	p, _ := InboxPath("x", "bob")
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(`[{"msg":"hello"}]`), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}
	if err := EnsureInbox("x", "bob"); err != nil {
		t.Fatalf("EnsureInbox: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != `[{"msg":"hello"}]` {
		t.Fatalf("EnsureInbox clobbered existing content: %s", got)
	}
}

func TestLoadTeamConfig_MissingReturnsErrTeamNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, err := LoadTeamConfig("nope")
	if err == nil {
		t.Fatal("LoadTeamConfig: want error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "team not found") {
		t.Fatalf("error %q should mention team not found", got)
	}
}

func TestParseTeamConfig_PreservesUnknownFields(t *testing.T) {
	in := []byte(`{
  "leadSessionId": "abc",
  "members": [{"name": "x", "agentId":"x@t", "agentType":"general-purpose"}],
  "futureField": "preserve me"
}`)
	tc, err := parseTeamConfig(in)
	if err != nil {
		t.Fatalf("parseTeamConfig: %v", err)
	}
	if tc.Raw["futureField"] != "preserve me" {
		t.Fatalf("unknown field lost; Raw=%v", tc.Raw)
	}
	if tc.LeadSessionID != "abc" {
		t.Fatalf("LeadSessionID = %q, want abc", tc.LeadSessionID)
	}
	if len(tc.Members) != 1 || tc.Members[0].Name != "x" {
		t.Fatalf("members not parsed: %+v", tc.Members)
	}
}

// TestSpawn_JSONMarshalIsClean asserts that on successful spawn we don't
// leak any "" empty fields into the JSON output beyond what's documented.
func TestSpawn_JSONMarshalIsClean(t *testing.T) {
	f := newFixture(t)
	f.writeProvidersTOML("")
	f.writeFingerprint()

	res := Spawn(Request{
		Provider: "deepseek", AgentName: "w", Team: "t",
		Probe: false, AutoTeam: true,
	})
	if !res.OK {
		t.Fatalf("Spawn: %s", res.ErrorMsg)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(data)
	for _, banned := range []string{"error_code", "error_msg"} {
		if strings.Contains(s, banned) {
			t.Fatalf("success JSON should omit %q, got %s", banned, s)
		}
	}
	for _, need := range []string{"\"ok\":true", "agent_id", "pane_id"} {
		if !strings.Contains(s, need) {
			t.Fatalf("success JSON missing %q: %s", need, s)
		}
	}
}

// Make sure the config package is wired into this file so build doesn't
// strip the import (used implicitly through Spawn → WithTeamLock).
var _ = config.SchemaVersion
