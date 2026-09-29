package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/teammate"
)

// cliArgsEnv carries the child argv (joined by \x1f) for tests that re-run this
// test binary as cc-fleet: check and setup exit the process themselves.
const cliArgsEnv = "CCF_TEST_CLI_ARGS"

// cliChildMain turns the process into `cc-fleet <args>` when it is the child.
func cliChildMain() {
	if raw, ok := os.LookupEnv(cliArgsEnv); ok {
		os.Args = append([]string{"cc-fleet"}, strings.Split(raw, "\x1f")...)
		main()
		os.Exit(0)
	}
}

// cliSkipWindows skips tests of the teammate lane, which refuses on Windows.
func cliSkipWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
}

// cliHome is a hermetic HOME / CLAUDE_CONFIG_DIR / XDG_CONFIG_HOME for a child.
type cliHome struct{ home, claude, xdg string }

func cliNewHome(t *testing.T) cliHome {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := cliHome{filepath.Join(root, "home"), filepath.Join(root, "claude"), filepath.Join(root, "xdg")}
	for _, d := range []string{h.home, h.claude, h.xdg} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// cliRun runs `cc-fleet <args>` in a child test process confined to h, with
// the Claude Code / tmux session variables removed.
func cliRun(t *testing.T, test string, h cliHome, args ...string) (stdout string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	drop := map[string]bool{"TMUX": true, "TMUX_PANE": true, "TMUX_TMPDIR": true, "CLAUDECODE": true,
		"CLAUDE_CODE_SESSION_ID": true, "CLAUDE_PID": true, "CLAUDE_CODE_ENTRYPOINT": true,
		teammate.EnvTeammateCommand: true, teammate.EnvAgentTeams: true, "TERM_PROGRAM": true, "ITERM_SESSION_ID": true,
		"HOME": true, "USERPROFILE": true, "CLAUDE_CONFIG_DIR": true, "XDG_CONFIG_HOME": true}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !drop[k] {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "HOME="+h.home, "USERPROFILE="+h.home, "CLAUDE_CONFIG_DIR="+h.claude,
		"XDG_CONFIG_HOME="+h.xdg, cliArgsEnv+"="+strings.Join(args, "\x1f"))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run child: %v", err)
	}
	if errb.Len() != 0 {
		t.Errorf("cc-fleet %v: stderr = %q", args, errb.String())
	}
	return out.String(), code
}

// cliEnvelope decodes stdout as exactly one JSON line.
func cliEnvelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	line := strings.TrimSuffix(stdout, "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout has more than one line: %q", stdout)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("stdout is not one JSON object: %v (%q)", err, stdout)
	}
	return m
}

func cliKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestTeammateCheckEnvelope: outside a Claude Code session, check --json prints
// one failure envelope (protocol 1, warnings []) and exits 1; without --json it
// prints one human line.
func TestTeammateCheckEnvelope(t *testing.T) {
	cliChildMain()
	cliSkipWindows(t)
	h := cliNewHome(t)

	out, code := cliRun(t, "TestTeammateCheckEnvelope", h, "teammate", "check", "glm", "--no-probe", "--json")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, out)
	}
	env := cliEnvelope(t, out)
	want := []string{"detail", "error_code", "error_msg", "ok", "protocol", "suggestion", "warnings"}
	if got := cliKeys(env); !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
	if env["ok"] != false || env["protocol"] != float64(teammate.Protocol) ||
		env["error_code"] != teammate.CodeLaneUnavailable || env["detail"] != teammate.DetailNoLeadSession {
		t.Errorf("envelope = %v", env)
	}
	if w, ok := env["warnings"].([]any); !ok || len(w) != 0 {
		t.Errorf("warnings = %#v, want []", env["warnings"])
	}

	out, code = cliRun(t, "TestTeammateCheckEnvelope", h, "teammate", "check", "--no-probe")
	if code != 1 || strings.Count(out, "\n") != 1 ||
		!strings.Contains(out, teammate.CodeLaneUnavailable+"("+teammate.DetailNoLeadSession+")") {
		t.Errorf("human check: exit %d, stdout %q", code, out)
	}
}

// TestTeammateSetupNeedsYes: without --yes, setup (and --remove) lists what it
// would change, answers BAD_ARGS / "rerun with --yes" and writes nothing.
func TestTeammateSetupNeedsYes(t *testing.T) {
	cliChildMain()
	h := cliNewHome(t)
	cliSeedProvider(t, h)

	for _, args := range [][]string{
		{"teammate", "setup", "--json"},
		{"teammate", "setup", "--teammate-mode", "tmux", "--json"},
		{"teammate", "setup", "--remove", "--json"},
	} {
		out, code := cliRun(t, "TestTeammateSetupNeedsYes", h, args...)
		if code != 1 {
			t.Fatalf("%v: exit = %d, want 1", args, code)
		}
		env := cliEnvelope(t, out)
		if env["ok"] != false || env["error_code"] != teammate.CodeBadArgs || env["suggestion"] != "rerun with --yes" {
			t.Errorf("%v: envelope = %v", args, env)
		}
		if msg, _ := env["error_msg"].(string); !strings.Contains(msg, filepath.Join(h.claude, "settings.json")) {
			t.Errorf("%v: error_msg does not name the settings file: %q", args, msg)
		}
	}

	out, code := cliRun(t, "TestTeammateSetupNeedsYes", h, "teammate", "setup")
	if code != 1 || !strings.Contains(out, "would change:") || !strings.Contains(out, "rerun with --yes") {
		t.Errorf("human: exit %d, stdout %q", code, out)
	}

	out, code = cliRun(t, "TestTeammateSetupNeedsYes", h, "teammate", "setup", "--teammate-mode", "auto", "--yes", "--json")
	if env := cliEnvelope(t, out); code != 1 || env["error_code"] != teammate.CodeBadArgs {
		t.Errorf("bad mode: exit %d, envelope %v", code, env)
	}

	for _, p := range []string{
		filepath.Join(h.claude, "settings.json"),
		filepath.Join(h.claude, "agents"),
		filepath.Join(h.xdg, "cc-fleet", "bin", teammate.ShimFileName),
		filepath.Join(h.xdg, "cc-fleet", "onboarding.json"),
	} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a refused setup (err=%v)", p, err)
		}
	}
}

// TestTeammateSetupYesAndRemove runs setup --yes --json, then --remove --yes
// --json, checking the setup envelopes.
func TestTeammateSetupYesAndRemove(t *testing.T) {
	cliChildMain()
	cliSkipWindows(t)
	h := cliNewHome(t)
	cliSeedProvider(t, h)
	shim := filepath.Join(h.xdg, "cc-fleet", "bin", teammate.ShimFileName)

	out, code := cliRun(t, "TestTeammateSetupYesAndRemove", h, "teammate", "setup", "--teammate-mode", "tmux", "--yes", "--json")
	env := cliEnvelope(t, out)
	if code != 0 || env["ok"] != true || env["action"] != "setup" || env["shim"] != shim ||
		env["teammate_mode"] != "tmux" || env["mode_written"] != true || env["restart_required"] != true {
		t.Fatalf("setup: exit %d, envelope %v", code, env)
	}
	wantKeys := []string{"action", "agent_defs", "changed", "mode_written", "ok", "restart_required",
		"settings_path", "shim", "teammate_mode", "teammate_mode_source", "warnings"}
	if got := cliKeys(env); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("setup keys = %v, want %v", got, wantKeys)
	}
	if _, err := os.Stat(filepath.Join(h.claude, "agents", "ccf-glm.md")); err != nil {
		t.Errorf("definition missing: %v", err)
	}

	out, code = cliRun(t, "TestTeammateSetupYesAndRemove", h, "teammate", "setup", "--yes")
	if code != 0 || !strings.Contains(out, "nothing changed") {
		t.Errorf("rerun: exit %d, stdout %q", code, out)
	}

	out, code = cliRun(t, "TestTeammateSetupYesAndRemove", h, "teammate", "setup", "--remove", "--yes", "--json")
	env = cliEnvelope(t, out)
	if code != 0 || env["ok"] != true || env["action"] != "remove" {
		t.Fatalf("remove: exit %d, envelope %v", code, env)
	}
	if _, err := os.Stat(filepath.Join(h.claude, "agents", "ccf-glm.md")); !os.IsNotExist(err) {
		t.Errorf("definition still present (err=%v)", err)
	}
	if _, err := os.Stat(shim); err != nil {
		t.Errorf("shim must be kept: %v", err)
	}
}

// cliSeedProvider writes providers.toml under h with one enabled provider glm.
func cliSeedProvider(t *testing.T, h cliHome) {
	t.Helper()
	cfg := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{
		"glm": {Name: "glm", BaseURL: "https://glm.example.test/api/anthropic", ModelsEndpoint: "https://glm.example.test/v1/models",
			DefaultModel: "glm-4.6", SecretBackend: "file", SecretRef: "glm", Enabled: true},
	}}
	p := filepath.Join(h.xdg, "cc-fleet", "providers.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveToPath(cfg, p); err != nil {
		t.Fatal(err)
	}
}
