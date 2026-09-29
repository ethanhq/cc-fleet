//go:build !windows

package teammate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// launchRec records what the stubbed seams saw.
type launchRec struct {
	execs       int
	argv0       string
	argv, env   []string
	execErr     error
	psid        string // --parent-session-id handed to resolveLeadBinaryFn
	daemonCalls int
	daemonErr   error
	profCalls   int
	profErr     error
}

// launchStub replaces every launcher seam: bin is what the lead-binary resolver
// returns (resolveErr makes it fail), the profile writer returns "/prof/<p>.json".
func launchStub(t *testing.T, bin string, resolveErr error) *launchRec {
	t.Helper()
	rec := &launchRec{}
	oldExec, oldDaemon, oldProf, oldResolve := execFn, ensureDaemonFn, writeProfileFn, resolveLeadBinaryFn
	t.Cleanup(func() {
		execFn, ensureDaemonFn, writeProfileFn, resolveLeadBinaryFn = oldExec, oldDaemon, oldProf, oldResolve
	})
	execFn = func(argv0 string, argv, env []string) error {
		rec.execs++
		rec.argv0, rec.argv, rec.env = argv0, slices.Clone(argv), slices.Clone(env)
		return rec.execErr
	}
	ensureDaemonFn = func(*config.Provider) error { rec.daemonCalls++; return rec.daemonErr }
	writeProfileFn = func(v *config.Provider) (string, error) {
		rec.profCalls++
		if rec.profErr != nil {
			return "", rec.profErr
		}
		return "/prof/" + v.Name + ".json", nil
	}
	resolveLeadBinaryFn = func(psid string) (string, error) {
		rec.psid = psid
		return bin, resolveErr
	}
	return rec
}

// launchRun runs the unix launcher and returns its exit code, stdout and stderr.
func launchRun(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := launchMain(args, &out, &errb)
	return code, out.String(), errb.String()
}

func launchSaveConfig(t *testing.T, providers ...*config.Provider) {
	t.Helper()
	cfg := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{}}
	for _, p := range providers {
		cfg.Providers[p.Name] = p
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func launchProvider(name string) *config.Provider {
	return &config.Provider{
		Name: name, BaseURL: "https://api.example.invalid/anthropic", ModelsEndpoint: "https://api.example.invalid/v1/models",
		DefaultModel: name + "-def", StrongModel: name + "-strong", FastModel: name + "-fast",
		SecretBackend: "file", SecretRef: name, Enabled: true,
	}
}

// launchTree lists every file under the hermetic dirs with size and mtime, so a
// test can assert that nothing was written.
func launchTree(t *testing.T, h testHome) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, root := range []string{h.Home, h.ClaudeDir, h.ConfigHome} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files[p] = fi.Mode().String() + " " + strconv.FormatInt(fi.Size(), 10) + " " + fi.ModTime().String()
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return files
}

// launchWriteSession writes sessions/<our pid>.json the way CC does, with a
// procStart that matches this process (or not, when stale).
func launchWriteSession(t *testing.T, h testHome, sessionID, version string, stale bool) {
	t.Helper()
	pid := os.Getpid()
	tok, ok := procintrospect.ProcStart(pid)
	if !ok {
		t.Skip("cannot read this process's start time")
	}
	procStart := tok
	if runtime.GOOS == "darwin" {
		epoch, err := strconv.ParseInt(tok, 10, 64)
		if err != nil {
			t.Fatalf("ProcStart token %q: %v", tok, err)
		}
		if stale {
			epoch++
		}
		procStart = time.Unix(epoch, 0).UTC().Format("Mon Jan _2 15:04:05 2006")
	} else if stale {
		procStart += "0"
	}
	data, err := json.Marshal(map[string]any{
		"pid": pid, "sessionId": sessionID, "cwd": h.Home, "startedAt": time.Now().UnixMilli(),
		"procStart": procStart, "version": version, "kind": "interactive", "entrypoint": "cli",
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(h.ClaudeDir, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// launchAssertFailure checks the failure contract: exit 1, nothing on stdout,
// exactly one stderr line carrying code and agentID.
func launchAssertFailure(t *testing.T, code int, stdout, stderr, wantCode, wantID string) {
	t.Helper()
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.HasSuffix(stderr, "\n") || strings.Count(stderr, "\n") != 1 {
		t.Fatalf("stderr = %q, want exactly one line", stderr)
	}
	gotCode, gotID, ok := ParseFailureLine(strings.TrimSuffix(stderr, "\n"))
	if !ok || gotCode != wantCode || gotID != wantID {
		t.Errorf("failure line %q parsed as (%q, %q, %v), want (%q, %q)", stderr, gotCode, gotID, ok, wantCode, wantID)
	}
}

func TestShimProtocolHandshake(t *testing.T) {
	h := hermeticHome(t)
	rec := launchStub(t, "/nonexistent/claude", nil)
	before := launchTree(t, h)

	code, out, errOut := launchRun("--shim-protocol", "1")
	if code != 0 || out != "1\n" || errOut != "" {
		t.Errorf("--shim-protocol 1 = (%d, %q, %q), want (0, \"1\\n\", \"\")", code, out, errOut)
	}
	for _, n := range []string{"2", "0", ""} {
		code, out, errOut = launchRun("--shim-protocol", n)
		if code != 3 || out != "" || errOut != "" {
			t.Errorf("--shim-protocol %q = (%d, %q, %q), want (3, \"\", \"\")", n, code, out, errOut)
		}
	}
	if rec.execs != 0 {
		t.Errorf("handshake exec'd %d times", rec.execs)
	}
	if after := launchTree(t, h); !reflect.DeepEqual(before, after) {
		t.Errorf("handshake wrote files: before %v, after %v", before, after)
	}
}

func TestPassthroughUnchanged(t *testing.T) {
	h := hermeticHome(t)
	// The real resolver: no session, so the claude on PATH.
	claude := writeFakeClaude(t, h.Bin, "echo '2.1.281 (Claude Code)'\n")
	rec := launchStub(t, "", nil)
	resolveLeadBinaryFn = resolveLeadBinary
	ensureDaemonFn = func(*config.Provider) error { t.Error("passthrough ensured a daemon"); return nil }
	writeProfileFn = func(*config.Provider) (string, error) { t.Error("passthrough wrote a profile"); return "", nil }
	t.Setenv("ANTHROPIC_API_KEY", "LEAK-passthrough")
	t.Setenv("ANTHROPIC_MODEL", "lead-model")
	t.Setenv("CLAUDECODE", "1")

	for _, args := range [][]string{
		{"--agent-id", "w@session-1", "--agent-name", "w", "--team-name", "session-1", "--agent-type", "general-purpose",
			"--settings", "/lead/flag.json", "--model", "opus", "--effort", "high", "--parent-session-id", "sid", "--dangerously-skip-permissions"},
		{"--agent-id=w@session-1", "--agent-type=Explore", "--settings=/lead/flag.json", "--model=sonnet", "--plan-mode-required"},
		{"--agent-id", "w@session-1", "--model", "opus"}, // no --agent-type at all
		{"--agent-id", "w@session-1", "--agent-type", "ccfx-p"},
		{},
	} {
		before := launchTree(t, h)
		env := os.Environ()
		code, out, errOut := launchRun(args...)
		if code != 0 || out != "" || errOut != "" {
			t.Fatalf("%q = (%d, %q, %q), want a silent exec", args, code, out, errOut)
		}
		if want := append([]string{claude}, args...); rec.argv0 != claude || !reflect.DeepEqual(rec.argv, want) {
			t.Errorf("%q exec'd %q %q, want %q %q", args, rec.argv0, rec.argv, claude, want)
		}
		if !reflect.DeepEqual(rec.env, env) {
			t.Errorf("%q changed the env", args)
		}
		if after := launchTree(t, h); !reflect.DeepEqual(before, after) {
			t.Errorf("%q wrote files: before %v, after %v", args, before, after)
		}
	}
}

func TestRouteRewritesArgv(t *testing.T) {
	h := hermeticHome(t)
	claude := writeFakeClaude(t, h.Bin, "exit 0\n")
	eff := launchProvider("eff")
	eff.Effort = "high"
	daemon := launchProvider("oai")
	daemon.Protocol, daemon.BaseURL, daemon.UpstreamURL = config.ProtocolOpenAIChat, "http://127.0.0.1:18431", "https://api.example.invalid/v1"
	launchSaveConfig(t, launchProvider("p"), eff, daemon)

	// Identity and everything unknown to the launcher stays in place.
	common := []string{"--agent-name", "w", "--team-name", "session-1", "--agent-color", "blue",
		"--dangerously-skip-permissions", "--permission-mode", "acceptEdits", "--plan-mode-required",
		"--plugin-dir", "/plug", "--mcp-config", "/m.json", "--strict-mcp-config", "--future-flag", "x"}

	cases := []struct {
		name    string
		args    []string
		keep    []string // expected argv before the appended --settings/--model
		psid    string
		model   string
		profile string
		daemons int
	}{
		{
			name: "space form, default slot, effort kept",
			args: append([]string{"--agent-id", "w@session-1", "--parent-session-id", "S1", "--agent-type", "ccf-p",
				"--settings", "/lead.json", "--model", "ccf-p", "--effort", "high"}, common...),
			keep:  append([]string{"--agent-id", "w@session-1", "--parent-session-id", "S1", "--effort", "high"}, common...),
			psid:  "S1",
			model: "p-def", profile: "/prof/p.json",
		},
		{
			name: "equals form, strong slot, effort kept",
			args: append([]string{"--agent-id=w@session-1", "--parent-session-id=S2", "--agent-type=ccf-p.strong",
				"--settings=/lead.json", "--model=ccf-p.strong", "--effort=high"}, common...),
			keep:  append([]string{"--agent-id=w@session-1", "--parent-session-id=S2", "--effort=high"}, common...),
			psid:  "S2",
			model: "p-strong", profile: "/prof/p.json",
		},
		{
			name: "mixed forms, fast slot, repeated flags all stripped",
			args: append(append([]string{"--model", "a", "--agent-id", "w@session-1"}, common...),
				"--agent-type=ccf-p.fast", "--settings", "/lead.json", "--model=b", "--settings=/other.json"),
			keep:  append([]string{"--agent-id", "w@session-1"}, common...),
			model: "p-fast", profile: "/prof/p.json",
		},
		{
			name:  "provider effort strips --effort (space form)",
			args:  []string{"--agent-id", "w@session-1", "--effort", "low", "--agent-type", "ccf-eff", "--plan-mode-required"},
			keep:  []string{"--agent-id", "w@session-1", "--plan-mode-required"},
			model: "eff-def", profile: "/prof/eff.json",
		},
		{
			name:  "provider effort strips --effort (equals form)",
			args:  []string{"--agent-id", "w@session-1", "--effort=max", "--agent-type", "ccf-eff.strong"},
			keep:  []string{"--agent-id", "w@session-1"},
			model: "eff-strong", profile: "/prof/eff.json",
		},
		{
			name:  "daemon-backed provider ensures its proxy",
			args:  []string{"--agent-id", "w@session-1", "--agent-type", "ccf-oai", "--model", "ccf-oai"},
			keep:  []string{"--agent-id", "w@session-1"},
			model: "oai-def", profile: "/prof/oai.json", daemons: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := launchStub(t, claude, nil)
			code, out, errOut := launchRun(tc.args...)
			if code != 0 || out != "" || errOut != "" {
				t.Fatalf("= (%d, %q, %q), want a silent exec", code, out, errOut)
			}
			want := append(append([]string{claude}, tc.keep...), "--settings", tc.profile, "--model", tc.model)
			if rec.execs != 1 || rec.argv0 != claude || !reflect.DeepEqual(rec.argv, want) {
				t.Errorf("exec'd %q\n got argv %q\nwant argv %q", rec.argv0, rec.argv, want)
			}
			if rec.psid != tc.psid {
				t.Errorf("resolver got parent session %q, want %q", rec.psid, tc.psid)
			}
			if rec.daemonCalls != tc.daemons || rec.profCalls != 1 {
				t.Errorf("daemon calls %d, profile writes %d; want %d, 1", rec.daemonCalls, rec.profCalls, tc.daemons)
			}
		})
	}
}

func TestRouteEnvScrub(t *testing.T) {
	h := hermeticHome(t)
	claude := writeFakeClaude(t, h.Bin, "exit 0\n")
	launchSaveConfig(t, launchProvider("p"))
	rec := launchStub(t, claude, nil)
	writeProfileFn = func(v *config.Provider) (string, error) { return profile.WriteForProvider(v, "") }

	scrubbed := map[string]string{
		"ANTHROPIC_API_KEY": "LEAK-api", "ANTHROPIC_AUTH_TOKEN": "LEAK-auth", "ANTHROPIC_BASE_URL": "https://lead.invalid",
		"ANTHROPIC_CUSTOM_HEADERS": "LEAK-hdr", "CLAUDE_CODE_OAUTH_TOKEN": "LEAK-oauth", "CLAUDE_CODE_USE_BEDROCK": "1",
		"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_BEDROCK_BASE_URL": "x", "ANTHROPIC_VERTEX_PROJECT_ID": "x",
		"AWS_BEARER_TOKEN_BEDROCK": "LEAK-aws", "ANTHROPIC_MODEL": "lead-model", "CLAUDE_CODE_SUBAGENT_MODEL": "x",
		EnvSubagentModelForce: "1", "CLAUDE_CODE_HOST_CREDS_FILE": "/x",
	}
	kept := map[string]string{"CLAUDECODE": "1", EnvAgentTeams: "1", "LAUNCH_TEST_KEEP": "ok"}
	for k, v := range scrubbed {
		t.Setenv(k, v)
	}
	for k, v := range kept {
		t.Setenv(k, v)
	}
	before := launchTree(t, h)
	env := os.Environ()

	code, _, errOut := launchRun("--agent-id", "w@session-1", "--agent-type", "ccf-p")
	if code != 0 || errOut != "" || rec.execs != 1 {
		t.Fatalf("= (%d, %q), execs %d; want a silent exec", code, errOut, rec.execs)
	}
	if want := childenv.CleanForTeammate(env); !reflect.DeepEqual(rec.env, want) {
		t.Errorf("env is not CleanForTeammate(os.Environ())")
	}
	got := map[string]string{}
	for _, kv := range rec.env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k := range scrubbed {
		if _, ok := got[k]; ok {
			t.Errorf("%s reached the teammate", k)
		}
	}
	for k, v := range kept {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if strings.Contains(strings.Join(rec.env, "\n")+strings.Join(rec.argv, "\n"), "LEAK-") {
		t.Errorf("a credential marker reached the teammate")
	}

	// The profile is the only file the launcher writes.
	prof, err := profile.ProfilePath("p")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rec.argv); n < 4 || rec.argv[n-4] != "--settings" || rec.argv[n-3] != prof {
		t.Errorf("argv %q does not end with --settings %s", rec.argv, prof)
	}
	after := launchTree(t, h)
	for p := range after {
		// New entries may only be the profile and the directories holding it.
		if _, ok := before[p]; !ok && p != prof && !strings.HasPrefix(prof, p+string(filepath.Separator)) {
			t.Errorf("launcher wrote %s", p)
		}
	}
	if _, ok := after[prof]; !ok {
		t.Errorf("profile %s was not written", prof)
	}
}

func TestLeadBinaryPriority(t *testing.T) {
	h := hermeticHome(t)
	onPath := writeFakeClaude(t, h.Bin, "echo '2.1.281 (Claude Code)'\n")
	versions := filepath.Join(h.Home, ".local", "share", "claude", "versions")
	exact := writeScript(t, filepath.Join(versions, "2.1.290"), "exit 0\n")

	cases := []struct {
		name          string
		session       string // "" = no session file
		version       string
		stale         bool
		parentSession string
		want          string
	}{
		{"live session with versions file wins over PATH", "S", "2.1.290", false, "S", exact},
		{"live session without versions file falls back to PATH", "S", "2.1.100", false, "S", onPath},
		{"stale session (procStart differs) is ignored", "S", "2.1.290", true, "S", onPath},
		{"other session id falls back to PATH", "S", "2.1.290", false, "T", onPath},
		{"no --parent-session-id falls back to PATH", "S", "2.1.290", false, "", onPath},
		{"no session file falls back to PATH", "", "", false, "S", onPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.RemoveAll(filepath.Join(h.ClaudeDir, "sessions"))
			if tc.session != "" {
				launchWriteSession(t, h, tc.session, tc.version, tc.stale)
			}
			got, err := resolveLeadBinary(tc.parentSession)
			if err != nil || got != tc.want {
				t.Errorf("resolveLeadBinary(%q) = %q, %v; want %q", tc.parentSession, got, err, tc.want)
			}
		})
	}

	// Nothing anywhere: the launcher reports CLAUDE_NOT_FOUND and does not exec.
	_ = os.RemoveAll(filepath.Join(h.ClaudeDir, "sessions"))
	if err := os.Remove(onPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(versions); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLeadBinary("S"); err == nil {
		t.Fatalf("resolveLeadBinary with no claude anywhere succeeded")
	}
	rec := launchStub(t, "", nil)
	resolveLeadBinaryFn = resolveLeadBinary
	code, out, errOut := launchRun("--agent-id", "w@session-1", "--agent-type", "general-purpose")
	launchAssertFailure(t, code, out, errOut, CodeClaudeNotFound, "w@session-1")
	if rec.execs != 0 {
		t.Errorf("exec'd without a claude")
	}
}

func TestRefuseSelfExec(t *testing.T) {
	h := hermeticHome(t)
	launchSaveConfig(t, launchProvider("p"))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shim, err := ShimPath()
	if err != nil {
		t.Fatal(err)
	}
	writeScript(t, shim, "exit 0\n")
	selfLink := filepath.Join(h.Bin, "claude-self")
	shimLink := filepath.Join(h.Bin, "claude-shim")
	if err := os.Symlink(self, selfLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shim, shimLink); err != nil {
		t.Fatal(err)
	}
	claude := writeFakeClaude(t, h.Bin, "exit 0\n")

	for _, typ := range []string{"general-purpose", "ccf-p"} {
		for _, bin := range []string{self, selfLink, shim, shimLink} {
			rec := launchStub(t, bin, nil)
			code, out, errOut := launchRun("--agent-id", "w@session-1", "--agent-type", typ)
			launchAssertFailure(t, code, out, errOut, CodeClaudeNotFound, "w@session-1")
			if rec.execs != 0 || rec.profCalls != 0 {
				t.Errorf("%s via %s: execs %d, profile writes %d; want 0, 0", typ, bin, rec.execs, rec.profCalls)
			}
		}
		rec := launchStub(t, claude, nil)
		if code, _, errOut := launchRun("--agent-id", "w@session-1", "--agent-type", typ); code != 0 || rec.execs != 1 {
			t.Errorf("%s via the real claude: (%d, %q), execs %d; want an exec", typ, code, errOut, rec.execs)
		}
	}
}

// TestFxtmRefuseSelfOnPathBeforeVersionProbe: with the real resolver, a PATH
// `claude` that links to the shim is refused before anything runs it (a
// `--version` probe would recurse into the launcher).
func TestFxtmRefuseSelfOnPathBeforeVersionProbe(t *testing.T) {
	h := hermeticHome(t)
	shim, err := ShimPath()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(h.Home, "shim.ran")
	writeScript(t, shim, ": > '"+marker+"'\necho '2.1.281 (Claude Code)'\n")
	if err := os.Symlink(shim, filepath.Join(h.Bin, "claude")); err != nil {
		t.Fatal(err)
	}

	for _, typ := range []string{"general-purpose", "ccf-p"} {
		launchSaveConfig(t, launchProvider("p"))
		rec := launchStub(t, "", nil)
		resolveLeadBinaryFn = resolveLeadBinary
		code, out, errOut := launchRun("--agent-id", "w@session-1", "--agent-type", typ)
		launchAssertFailure(t, code, out, errOut, CodeClaudeNotFound, "w@session-1")
		if rec.execs != 0 || rec.profCalls != 0 {
			t.Errorf("%s: execs %d, profile writes %d; want 0, 0", typ, rec.execs, rec.profCalls)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%s: the shim on PATH was executed", typ)
		}
	}
}

func TestRouteFailureLines(t *testing.T) {
	h := hermeticHome(t)
	claude := writeFakeClaude(t, h.Bin, "exit 0\n")
	off := launchProvider("off")
	off.Enabled = false
	daemon := launchProvider("oai")
	daemon.Protocol, daemon.BaseURL, daemon.UpstreamURL = config.ProtocolOpenAIChat, "http://127.0.0.1:18432", "https://api.example.invalid/v1"
	launchSaveConfig(t, launchProvider("p"), off, daemon)
	t.Setenv("ANTHROPIC_API_KEY", "LEAK-key")
	const id = "w@session-1"

	cases := []struct {
		name   string
		args   []string
		setup  func(rec *launchRec)
		code   string
		id     string
		execs  int
		broken bool // providers.toml is garbage
	}{
		{name: "bad slot", args: []string{"--agent-id", id, "--agent-type", "ccf-p.bogus"}, code: CodeBadArgs, id: id},
		{name: "reserved provider", args: []string{"--agent-id", id, "--agent-type", "ccf-claude"}, code: CodeBadArgs, id: id},
		{name: "empty provider", args: []string{"--agent-id", id, "--agent-type=ccf-"}, code: CodeBadArgs, id: id},
		{name: "invalid provider name", args: []string{"--agent-id", id, "--agent-type", "ccf-1bad"}, code: CodeBadArgs, id: id},
		{name: "missing agent id", args: []string{"--agent-type", "ccf-p"}, code: CodeBadArgs, id: "-"},
		{name: "agent id without team", args: []string{"--agent-id", "worker", "--agent-type", "ccf-p"}, code: CodeBadArgs, id: "worker"},
		{name: "agent id with bad team", args: []string{"--agent-id=w@a/b", "--agent-type", "ccf-p"}, code: CodeBadArgs, id: "w@a/b"},
		{name: "agent id with bad name", args: []string{"--agent-id", "%1@session-1", "--agent-type", "ccf-p"}, code: CodeBadArgs, id: "%1@session-1"},
		{name: "config unreadable", args: []string{"--agent-id", id, "--agent-type", "ccf-p"}, code: CodeConfigLoadFailed, id: id, broken: true},
		{name: "unknown provider", args: []string{"--agent-id", id, "--agent-type", "ccf-nope"}, code: CodeUnknownProvider, id: id},
		{name: "disabled provider", args: []string{"--agent-id", id, "--agent-type", "ccf-off.strong"}, code: CodeProviderDisabled, id: id},
		{
			name: "lead binary not found", args: []string{"--agent-id", id, "--agent-type", "ccf-p"}, code: CodeClaudeNotFound, id: id,
			setup: func(*launchRec) {
				resolveLeadBinaryFn = func(string) (string, error) { return "", errors.New("no claude") }
			},
		},
		{
			name: "passthrough binary not found", args: []string{"--agent-id", id, "--agent-type", "general-purpose"}, code: CodeClaudeNotFound, id: id,
			setup: func(*launchRec) {
				resolveLeadBinaryFn = func(string) (string, error) { return "", errors.New("no claude") }
			},
		},
		{
			name: "proxy daemon fails", args: []string{"--agent-id", id, "--agent-type", "ccf-oai"}, code: CodeCodexProxyUnavailable, id: id,
			setup: func(rec *launchRec) { rec.daemonErr = errors.New("port busy") },
		},
		{
			name: "profile write fails", args: []string{"--agent-id", id, "--agent-type", "ccf-p"}, code: CodeProfileWriteFailed, id: id,
			setup: func(rec *launchRec) { rec.profErr = errors.New("read-only") },
		},
		{
			name: "exec fails", args: []string{"--agent-id", id, "--agent-type", "ccf-p"}, code: CodeInternal, id: id, execs: 1,
			setup: func(rec *launchRec) { rec.execErr = errors.New("exec format error") },
		},
		{
			name: "passthrough exec fails", args: []string{"--agent-id", id}, code: CodeInternal, id: id, execs: 1,
			setup: func(rec *launchRec) { rec.execErr = errors.New("exec format error") },
		},
	}
	providers, err := config.ProvidersPath()
	if err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(providers)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := launchStub(t, claude, nil)
			if tc.setup != nil {
				tc.setup(rec)
			}
			data := good
			if tc.broken {
				data = []byte("version = [\n")
			}
			if err := os.WriteFile(providers, data, 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := launchRun(tc.args...)
			launchAssertFailure(t, code, out, errOut, tc.code, tc.id)
			if rec.execs != tc.execs {
				t.Errorf("execs = %d, want %d", rec.execs, tc.execs)
			}
			if tc.code == CodeCodexProxyUnavailable && rec.profCalls != 0 {
				t.Errorf("profile written after the proxy failed")
			}
			if strings.Contains(errOut, "LEAK-") {
				t.Errorf("failure line leaks an env value: %q", errOut)
			}
		})
	}
}
