package teammate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/providerclass"
)

const (
	evalSessionID = "7c8f769b-1111-2222-3333-444455556666"
	evalTeam      = "session-7c8f769b"
	evalLeadPID   = 4242
)

// evalFixture is a lead session in which a ccf-glm teammate passes every step
// of Evaluate: CC 2.1.281, a session team, cc-fleet's shim in the env and in
// user settings, managed definitions older than the lead, teammateMode tmux in
// user settings, and TMUX set. Tests break one piece at a time.
type evalFixture struct {
	h        testHome
	proj     string // the lead's cwd
	managed  string // managedSettingsDirs() in this test
	shim     string
	sess     leadsession.Session
	detectOK bool
	argv     []string
	probe    providerclass.Probe
	proxyErr error
	probed   []string // provider names probeFn saw
	proxied  []string // provider names ensureProxyFn saw
}

// evalProviders is the fixture's providers.toml: glm (strong differs from the
// default, fast does not) and a disabled provider.
func evalProviders() *config.Config {
	return &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{
		"glm": {
			Name: "glm", BaseURL: "https://glm.example.test/api/anthropic", ModelsEndpoint: "https://glm.example.test/v1/models",
			DefaultModel: "glm-4.6", StrongModel: "glm-5", FastModel: "glm-4.6",
			SecretBackend: "file", SecretRef: "glm", Enabled: true,
		},
		"off": {
			Name: "off", BaseURL: "https://off.example.test/api/anthropic", ModelsEndpoint: "https://off.example.test/v1/models",
			DefaultModel: "off-1", SecretBackend: "file", SecretRef: "off", Enabled: false,
		},
	}}
}

// evalSetup builds the passing fixture and installs the package seams. It
// simulates a darwin lead but relies on real POSIX file modes (the shim's x
// bit), so it is skipped on Windows.
func evalSetup(t *testing.T) *evalFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture needs POSIX executable bits for the shim")
	}
	h := hermeticHome(t)
	f := &evalFixture{
		h:        h,
		proj:     filepath.Join(h.Home, "proj"),
		managed:  filepath.Join(h.Home, "managed"),
		detectOK: true,
		argv:     []string{"claude"},
	}
	for _, d := range []string{f.proj, f.managed} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli()
	f.sess = leadsession.Session{
		PID: evalLeadPID, SessionID: evalSessionID, Cwd: f.proj, StartedAt: now - 60_000,
		ProcStart: "1", Version: "2.1.281", Entrypoint: "cli",
	}

	oldGOOS, oldDetect, oldCmdline, oldProxy, oldProbe, oldManaged := evalGOOS, detectSessionFn, leadCmdlineFn, ensureProxyFn, probeFn, managedSettingsDirs
	t.Cleanup(func() {
		evalGOOS, detectSessionFn, leadCmdlineFn, ensureProxyFn, probeFn, managedSettingsDirs = oldGOOS, oldDetect, oldCmdline, oldProxy, oldProbe, oldManaged
	})
	evalGOOS = "darwin"
	detectSessionFn = func() (leadsession.Session, bool) { return f.sess, f.detectOK }
	leadCmdlineFn = func(pid int) ([]string, error) {
		if pid != f.sess.PID {
			return nil, errors.New("unexpected pid")
		}
		return f.argv, nil
	}
	ensureProxyFn = func(v *config.Provider, _ *diag.Logger) error {
		f.proxied = append(f.proxied, v.Name)
		return f.proxyErr
	}
	probeFn = func(v *config.Provider) providerclass.Probe {
		f.probed = append(f.probed, v.Name)
		return f.probe
	}
	managedSettingsDirs = func() []string { return []string{f.managed} }

	evalWriteJSON(t, filepath.Join(h.ClaudeDir, "teams", evalTeam, "config.json"), map[string]any{
		"name": evalTeam, "createdAt": f.sess.StartedAt - 250, // created before the session registers
		"leadAgentId": "team-lead@" + evalTeam, "leadSessionId": evalSessionID,
		"members": []map[string]any{{"agentId": "team-lead@" + evalTeam, "name": "team-lead", "cwd": f.proj}},
	})

	cfg := evalProviders()
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save providers.toml: %v", err)
	}
	if _, err := SyncAgentDefs(cfg); err != nil {
		t.Fatalf("sync agent defs: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	for _, name := range []string{"ccf-glm.md", "ccf-glm.strong.md"} {
		if err := os.Chtimes(filepath.Join(h.ClaudeDir, "agents", name), old, old); err != nil {
			t.Fatal(err)
		}
	}

	shim, _, err := WriteShim("")
	if err != nil {
		t.Fatalf("write shim: %v", err)
	}
	f.shim = shim
	t.Setenv(EnvTeammateCommand, shim)
	evalWriteJSON(t, filepath.Join(h.ClaudeDir, "settings.json"), map[string]any{
		"env":          map[string]any{EnvTeammateCommand: shim, EnvAgentTeams: "1"},
		"teammateMode": "tmux",
	})
	t.Setenv("TMUX", "/private/tmp/tmux-501/default,1234,0")
	return f
}

// evalWriteJSON writes v as JSON at path, creating parent directories.
func evalWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	evalWriteFile(t, path, string(data))
}

// evalWriteFile writes s at path, creating parent directories.
func evalWriteFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// evalCheck is the Input `teammate check glm --slot strong --no-probe` would build.
func evalCheck() Input { return Input{Provider: "glm", Slot: SlotStrong, Prepare: false, Probe: false} }

// evalAssertFailure locks the JSON of a failed Result: exactly these keys,
// ok=false, protocol=1, warnings=[], and the expected code and detail.
func evalAssertFailure(t *testing.T, r Result, code, detail string) {
	t.Helper()
	if r.ErrorCode != code || r.Detail != detail {
		t.Fatalf("got %s/%q (%s — %s), want %s/%q", r.ErrorCode, r.Detail, r.ErrorMsg, r.Suggestion, code, detail)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	want := []string{"error_code", "error_msg", "ok", "protocol", "suggestion", "warnings"}
	if detail != "" {
		want = append(want, "detail")
	}
	sort.Strings(want)
	var got []string
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JSON keys = %v, want %v\n%s", got, want, data)
	}
	if string(m["ok"]) != "false" || string(m["protocol"]) != "1" || string(m["warnings"]) != "[]" {
		t.Errorf("ok/protocol/warnings = %s/%s/%s, want false/1/[]", m["ok"], m["protocol"], m["warnings"])
	}
	if r.ErrorMsg == "" || r.Suggestion == "" {
		t.Errorf("error_msg and suggestion must be set: %+v", r)
	}
}

func TestEvaluateEveryCode(t *testing.T) {
	agentCall := func(c AgentCall) Input {
		if c.SubagentType == "" {
			c.SubagentType = "ccf-glm"
		}
		return Input{Provider: "glm", Slot: SlotDefault, AgentCall: &c}
	}
	saveConfig := func(mut func(*config.Config)) func(*testing.T, *evalFixture) {
		return func(t *testing.T, _ *evalFixture) {
			cfg := evalProviders()
			mut(cfg)
			if err := config.Save(cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	userMode := func(mode string) func(*testing.T, *evalFixture) {
		return func(t *testing.T, f *evalFixture) {
			evalWriteJSON(t, filepath.Join(f.h.ClaudeDir, "settings.json"), map[string]any{
				"env": map[string]any{EnvTeammateCommand: f.shim}, "teammateMode": mode,
			})
		}
	}
	cases := []struct {
		name   string
		setup  func(*testing.T, *evalFixture)
		in     Input
		code   string
		detail string
	}{
		{"missing name", nil, agentCall(AgentCall{Name: " "}), CodeBadAgentCall, DetailMissingName},
		{"model param", nil, agentCall(AgentCall{Name: "w", Model: "opus"}), CodeBadAgentCall, DetailModelParam},
		{"isolation", nil, agentCall(AgentCall{Name: "w", Isolation: "worktree"}), CodeBadAgentCall, DetailIsolation},
		{"cwd", nil, agentCall(AgentCall{Name: "w", Cwd: "/tmp"}), CodeBadAgentCall, DetailCwd},
		{"windows", func(_ *testing.T, _ *evalFixture) { evalGOOS = "windows" }, evalCheck(), CodeUnsupportedOnWindows, ""},
		{"no lead session", func(_ *testing.T, f *evalFixture) { f.detectOK = false }, evalCheck(), CodeLaneUnavailable, DetailNoLeadSession},
		{"cc too old", func(_ *testing.T, f *evalFixture) { f.sess.Version = "2.1.277" }, evalCheck(), CodeLaneUnavailable, DetailCCTooOld},
		{"cc version unknown", func(_ *testing.T, f *evalFixture) { f.sess.Version = "" }, evalCheck(), CodeLaneUnavailable, DetailCCTooOld},
		{"no session team", func(t *testing.T, f *evalFixture) {
			if err := os.RemoveAll(filepath.Join(f.h.ClaudeDir, "teams")); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeLaneUnavailable, DetailNoSessionTeam},
		{"zinc harbor", func(t *testing.T, f *evalFixture) {
			evalWriteFile(t, filepath.Join(f.h.ClaudeDir, ".claude.json"), `{"cachedGrowthBookFeatures":{"tengu_zinc_harbor":true}}`)
		}, evalCheck(), CodeLaneUnavailable, DetailDisabledByCC},
		{"launcher configured after start", func(t *testing.T, _ *evalFixture) { t.Setenv(EnvTeammateCommand, "") }, evalCheck(), CodeLeadRestart, DetailLauncher},
		{"launcher not configured", func(t *testing.T, f *evalFixture) {
			t.Setenv(EnvTeammateCommand, "")
			evalWriteJSON(t, filepath.Join(f.h.ClaudeDir, "settings.json"), map[string]any{"teammateMode": "tmux"})
		}, evalCheck(), CodeSetupRequired, DetailLauncherNotConfigured},
		{"launcher foreign", func(t *testing.T, f *evalFixture) {
			t.Setenv(EnvTeammateCommand, writeScript(t, filepath.Join(f.h.Bin, "other-launcher"), "exec claude \"$@\"\n"))
		}, evalCheck(), CodeSetupRequired, DetailLauncherForeign},
		{"shim not executable", func(t *testing.T, f *evalFixture) {
			if err := os.Chmod(f.shim, 0o644); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeSetupRequired, DetailShimBroken},
		{"shim pins a missing binary", func(t *testing.T, f *evalFixture) {
			if _, _, err := WriteShim(filepath.Join(f.h.Home, "gone", "cc-fleet")); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeSetupRequired, DetailShimBroken},
		{"shim missing", func(t *testing.T, f *evalFixture) {
			if err := os.Remove(f.shim); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeSetupRequired, DetailShimBroken},
		{"reserved provider", nil, Input{Provider: "claude"}, CodeBadArgs, ""},
		{"bad slot", nil, Input{Provider: "glm", Slot: "huge"}, CodeBadArgs, ""},
		{"bad provider name", nil, Input{Provider: "../x"}, CodeBadArgs, ""},
		{"config unparseable", func(t *testing.T, f *evalFixture) {
			evalWriteFile(t, filepath.Join(f.h.ConfigHome, "cc-fleet", "providers.toml"), "version = [\n")
		}, evalCheck(), CodeConfigLoadFailed, ""},
		{"no default provider", saveConfig(func(c *config.Config) {
			k := *c.Providers["glm"]
			k.Name, k.SecretRef = "kimi", "kimi"
			c.Providers["kimi"] = &k
		}), Input{}, "NO_DEFAULT_PROVIDER", ""},
		{"default provider unknown", saveConfig(func(c *config.Config) { c.DefaultProvider = "gone" }), Input{}, "DEFAULT_PROVIDER_UNKNOWN", ""},
		{"default provider disabled", saveConfig(func(c *config.Config) { c.DefaultProvider = "off" }), Input{}, "DEFAULT_PROVIDER_DISABLED", ""},
		{"default provider reserved", func(t *testing.T, f *evalFixture) {
			evalWriteFile(t, filepath.Join(f.h.ConfigHome, "cc-fleet", "providers.toml"), "version = 1\ndefault_provider = \"claude\"\n")
		}, Input{}, "DEFAULT_PROVIDER_RESERVED", ""},
		{"unknown provider", nil, Input{Provider: "nope"}, CodeUnknownProvider, ""},
		{"provider disabled", nil, Input{Provider: "off"}, CodeProviderDisabled, ""},
		{"agent def missing", func(t *testing.T, f *evalFixture) {
			if err := os.Remove(filepath.Join(f.h.ClaudeDir, "agents", "ccf-glm.strong.md")); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeSetupRequired, DetailAgentDefMissing},
		{"agent def foreign", func(t *testing.T, f *evalFixture) {
			evalWriteFile(t, filepath.Join(f.h.ClaudeDir, "agents", "ccf-glm.strong.md"), "---\nname: ccf-glm.strong\n---\nmine\n")
		}, evalCheck(), CodeSetupRequired, DetailAgentDefForeign},
		{"agent def shadowed", func(t *testing.T, f *evalFixture) {
			evalWriteFile(t, filepath.Join(f.proj, ".claude", "agents", "team", "x.md"), "---\nname: ccf-glm.strong\n---\nproject copy\n")
		}, evalCheck(), CodeSetupRequired, DetailAgentDefShadowed + ":" + "<proj>/.claude/agents/team/x.md"},
		{"teammateMode written after start", func(t *testing.T, f *evalFixture) {
			st := onboarding.State{TeammateLane: onboarding.TeammateLane{Enabled: true, ModeWrittenAt: f.sess.StartedAt + 1}}
			if err := st.Save(); err != nil {
				t.Fatal(err)
			}
		}, evalCheck(), CodeLeadRestart, DetailTeammateMode},
		{"mode in-process", userMode("in-process"), evalCheck(), CodeModeInProcess, DetailModeInProcess + ":" + SourceUser},
		{"mode default", func(t *testing.T, f *evalFixture) {
			evalWriteJSON(t, filepath.Join(f.h.ClaudeDir, "settings.json"), map[string]any{"env": map[string]any{EnvTeammateCommand: f.shim}})
		}, evalCheck(), CodeModeInProcess, DetailModeInProcess + ":" + SourceDefault},
		{"auto outside a pane terminal", func(t *testing.T, f *evalFixture) {
			userMode("auto")(t, f)
			t.Setenv("TMUX", "")
		}, evalCheck(), CodeModeInProcess, DetailAutoNoPaneTerminal},
		{"auto unguarded", func(t *testing.T, f *evalFixture) {
			userMode("auto")(t, f)
			f.argv = []string{"claude", "--fallback-model", "haiku"}
		}, evalCheck(), CodeModeInProcess, DetailAutoFallbackUnguarded + ":" + BlockerFallbackModel},
		{"mode unknown", userMode("sideways"), evalCheck(), CodeModeInProcess, DetailModeUnknown + ":sideways"},
		{"claude not found", nil, Input{Provider: "glm", Prepare: true}, CodeClaudeNotFound, ""},
		{"profile write failed", func(t *testing.T, f *evalFixture) {
			writeFakeClaude(t, f.h.Bin, "echo '2.1.281 (Claude Code)'\n")
			evalWriteFile(t, filepath.Join(f.h.Home, ".claude", "profiles"), "not a directory")
		}, Input{Provider: "glm", Prepare: true}, CodeProfileWriteFailed, ""},
		{"codex proxy unavailable", func(t *testing.T, f *evalFixture) {
			writeFakeClaude(t, f.h.Bin, "echo '2.1.281 (Claude Code)'\n")
			f.proxyErr = errors.New("port 17222 busy")
		}, Input{Provider: "glm", Prepare: true}, CodeCodexProxyUnavailable, ""},
		{"provider unreachable", func(_ *testing.T, f *evalFixture) {
			f.probe = providerclass.Probe{Block: true, Code: CodeProviderUnreachable, Msg: "dial tcp: refused", Suggestion: "run cc-fleet doctor"}
		}, Input{Provider: "glm", Probe: true}, CodeProviderUnreachable, ""},
		{"key invalid", func(_ *testing.T, f *evalFixture) {
			f.probe = providerclass.Probe{Block: true, Code: CodeKeyInvalid, Msg: "HTTP 401", Suggestion: "cc-fleet edit glm"}
		}, Input{Provider: "glm", Probe: true}, CodeKeyInvalid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := evalSetup(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			detail := strings.ReplaceAll(tc.detail, "<proj>", f.proj)
			evalAssertFailure(t, Evaluate(context.Background(), tc.in), tc.code, detail)
		})
	}
}

// TestFxtmEvaluateWindowsRefusal runs on every platform (evalSetup is skipped
// on Windows): step 1 refuses before any lead session or file is looked at.
func TestFxtmEvaluateWindowsRefusal(t *testing.T) {
	hermeticHome(t)
	oldGOOS, oldDetect := evalGOOS, detectSessionFn
	t.Cleanup(func() { evalGOOS, detectSessionFn = oldGOOS, oldDetect })
	evalGOOS = "windows"
	detectSessionFn = func() (leadsession.Session, bool) {
		t.Error("lead session detected after the platform refusal")
		return leadsession.Session{}, false
	}
	evalAssertFailure(t, Evaluate(context.Background(), evalCheck()), CodeUnsupportedOnWindows, "")
}

func TestEvaluateDesktopSession(t *testing.T) {
	f := evalSetup(t)
	f.sess.Entrypoint = "claude-desktop"
	// the desktop app never creates a session team
	if err := os.RemoveAll(filepath.Join(f.h.ClaudeDir, "teams")); err != nil {
		t.Fatal(err)
	}
	r := Evaluate(context.Background(), evalCheck())
	evalAssertFailure(t, r, CodeLaneUnavailable, DetailNoSessionTeam)
	if !strings.Contains(r.ErrorMsg, "entrypoint=claude-desktop") {
		t.Errorf("error_msg %q does not name the entrypoint", r.ErrorMsg)
	}

	// A terminal lead can inherit the desktop app's entrypoint, so every
	// entrypoint gets the same message: both causes, and a new claude in a
	// terminal inside tmux or iTerm2.
	for _, ep := range []string{"claude-desktop", "cli"} {
		f.sess.Entrypoint = ep
		r = Evaluate(context.Background(), evalCheck())
		evalAssertFailure(t, r, CodeLaneUnavailable, DetailNoSessionTeam)
		if !strings.Contains(r.ErrorMsg, "entrypoint="+ep) || !strings.Contains(r.ErrorMsg, "desktop app") || !strings.Contains(r.ErrorMsg, "agent teams are off") ||
			!strings.Contains(r.Suggestion, "start a new `claude`") || !strings.Contains(r.Suggestion, "teammate setup") {
			t.Errorf("%s no_session_team = %q / %q, want both causes and a new claude", ep, r.ErrorMsg, r.Suggestion)
		}
	}
}

func TestEvaluateRestartRequired(t *testing.T) {
	t.Run("launcher", func(t *testing.T) {
		evalSetup(t)
		// setup wrote the shim into user settings, but this lead's env predates it
		t.Setenv(EnvTeammateCommand, "")
		evalAssertFailure(t, Evaluate(context.Background(), evalCheck()), CodeLeadRestart, DetailLauncher)
	})
	t.Run("teammate_mode", func(t *testing.T) {
		f := evalSetup(t)
		st := onboarding.State{TeammateLane: onboarding.TeammateLane{Enabled: true, ModeWrittenAt: f.sess.StartedAt + 5_000}}
		if err := st.Save(); err != nil {
			t.Fatal(err)
		}
		evalAssertFailure(t, Evaluate(context.Background(), evalCheck()), CodeLeadRestart, DetailTeammateMode)

		// written before the lead started: no restart needed
		st.TeammateLane.ModeWrittenAt = f.sess.StartedAt - 5_000
		if err := st.Save(); err != nil {
			t.Fatal(err)
		}
		if r := Evaluate(context.Background(), evalCheck()); !r.OK {
			t.Fatalf("mode written before the lead: %+v", r)
		}
	})
}

// TestFxtmEvaluateModeFileNewerThanLead: with a sentinel blocker, a
// teammateMode read from a settings file changed after the lead started may
// not be the mode the lead runs, so a restart is required.
func TestFxtmEvaluateModeFileNewerThanLead(t *testing.T) {
	blocker := func(t *testing.T, f *evalFixture) {
		evalWriteFile(t, filepath.Join(f.proj, ".claude", "settings.json"), `{"availableModels":["sonnet"]}`)
	}
	age := func(path string, d time.Duration) func(*testing.T, *evalFixture) {
		return func(t *testing.T, f *evalFixture) {
			ts := time.UnixMilli(f.sess.StartedAt).Add(d)
			if err := os.Chtimes(strings.ReplaceAll(path, "<claude>", f.h.ClaudeDir), ts, ts); err != nil {
				t.Fatal(err)
			}
		}
	}
	userSettings := "<claude>/settings.json"
	cases := []struct {
		name    string
		setup   []func(*testing.T, *evalFixture)
		restart bool
	}{
		{"blocker, user file newer", []func(*testing.T, *evalFixture){blocker, age(userSettings, time.Second)}, true},
		{"no blocker, user file newer", []func(*testing.T, *evalFixture){age(userSettings, time.Second)}, false},
		{"blocker, user file older", []func(*testing.T, *evalFixture){blocker, age(userSettings, -time.Second)}, false},
		{"blocker, mode from the CLI", []func(*testing.T, *evalFixture){blocker, age(userSettings, time.Second),
			func(_ *testing.T, f *evalFixture) { f.argv = []string{"claude", "--teammate-mode", "tmux"} }}, false},
		{"blocker, mode from inline --settings", []func(*testing.T, *evalFixture){blocker, age(userSettings, time.Second),
			func(_ *testing.T, f *evalFixture) {
				f.argv = []string{"claude", "--settings", `{"teammateMode":"tmux"}`}
			}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := evalSetup(t)
			for _, s := range tc.setup {
				s(t, f)
			}
			r := Evaluate(context.Background(), evalCheck())
			if tc.restart {
				evalAssertFailure(t, r, CodeLeadRestart, DetailTeammateMode)
			} else if !r.OK {
				t.Fatalf("want ok, got %+v", r)
			}
		})
	}
}

func TestEvaluateZincHarbor(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" = no global config file
		blocked bool
	}{
		{"missing file", "", false},
		{"flag true", `{"cachedGrowthBookFeatures":{"tengu_zinc_harbor":true,"other":1},"teammateMode":"tmux"}`, true},
		{"flag false", `{"cachedGrowthBookFeatures":{"tengu_zinc_harbor":false}}`, false},
		{"flag string", `{"cachedGrowthBookFeatures":{"tengu_zinc_harbor":"true"}}`, false},
		{"no features", `{"numStartups":3}`, false},
		{"unparseable", `{"cachedGrowthBookFeatures":`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := evalSetup(t)
			if tc.content != "" {
				evalWriteFile(t, filepath.Join(f.h.ClaudeDir, ".claude.json"), tc.content)
			}
			r := Evaluate(context.Background(), evalCheck())
			if tc.blocked {
				evalAssertFailure(t, r, CodeLaneUnavailable, DetailDisabledByCC)
			} else if !r.OK {
				t.Fatalf("want ok, got %+v", r)
			}
		})
	}
}

func TestEvaluatePrepareAndProbeSeams(t *testing.T) {
	t.Run("check runs prepare and probe", func(t *testing.T) {
		f := evalSetup(t)
		writeFakeClaude(t, f.h.Bin, "echo '2.1.281 (Claude Code)'\n")
		r := Evaluate(context.Background(), Input{Provider: "glm", Slot: SlotStrong, Prepare: true, Probe: true})
		want := Result{
			OK: true, Protocol: 1, Provider: "glm", Slot: SlotStrong, Model: "glm-5", AgentType: "ccf-glm.strong",
			Team: evalTeam, LeadPID: evalLeadPID, CCVersion: "2.1.281", Entrypoint: "cli",
			TeammateMode: "tmux", TeammateModeSource: SourceUser, BackendHint: HintTmux,
			Launcher: f.shim, Warnings: []string{},
		}
		if !reflect.DeepEqual(r, want) {
			t.Fatalf("result\n got %+v\nwant %+v", r, want)
		}
		if !reflect.DeepEqual(f.probed, []string{"glm"}) || !reflect.DeepEqual(f.proxied, []string{"glm"}) {
			t.Errorf("probe/proxy seams saw %v / %v, want [glm] each", f.probed, f.proxied)
		}
		if _, err := os.Stat(filepath.Join(f.h.Home, ".claude", "profiles", "glm.json")); err != nil {
			t.Errorf("profile not written: %v", err)
		}
		data, _ := json.Marshal(r)
		if !strings.Contains(string(data), `"warnings":[]`) || !strings.Contains(string(data), `"protocol":1`) {
			t.Errorf("success JSON %s", data)
		}
	})
	t.Run("probe warning", func(t *testing.T) {
		f := evalSetup(t)
		f.probe = providerclass.Probe{Warn: "HTTP 503 from models endpoint"}
		r := Evaluate(context.Background(), Input{Provider: "glm", Probe: true})
		if !r.OK || !reflect.DeepEqual(r.Warnings, []string{WarnProviderProbe}) {
			t.Fatalf("got %+v, want ok with %s", r, WarnProviderProbe)
		}
	})
	t.Run("no prepare no probe", func(t *testing.T) {
		f := evalSetup(t)
		r := Evaluate(context.Background(), Input{Provider: "glm"})
		if !r.OK || r.AgentType != "ccf-glm" || r.Model != "glm-4.6" || r.Slot != SlotDefault {
			t.Fatalf("got %+v", r)
		}
		if len(f.probed)+len(f.proxied) != 0 {
			t.Errorf("seams called without Prepare/Probe: %v %v", f.probed, f.proxied)
		}
		if _, err := os.Stat(filepath.Join(f.h.Home, ".claude", "profiles")); !os.IsNotExist(err) {
			t.Errorf("profiles dir exists without Prepare: %v", err)
		}
	})
	t.Run("default provider and slot without its own definition", func(t *testing.T) {
		evalSetup(t)
		// glm is the sole enabled provider; its fast model equals the default,
		// so there is no ccf-glm.fast definition and check hands out ccf-glm.
		r := Evaluate(context.Background(), Input{Slot: SlotFast})
		if !r.OK || r.Provider != "glm" || r.Slot != SlotFast || r.AgentType != "ccf-glm" || r.Model != "glm-4.6" {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("launcher pinned to another binary warns", func(t *testing.T) {
		f := evalSetup(t)
		other := writeScript(t, filepath.Join(f.h.Home, "other", "cc-fleet"), "exit 0\n")
		if _, _, err := WriteShim(other); err != nil {
			t.Fatal(err)
		}
		r := Evaluate(context.Background(), evalCheck())
		if !r.OK || !reflect.DeepEqual(r.Warnings, []string{WarnLauncherBinaryDiffers}) {
			t.Fatalf("got %+v", r)
		}
	})
	t.Run("auto in tmux warns", func(t *testing.T) {
		f := evalSetup(t)
		f.argv = []string{"claude", "--teammate-mode=auto"}
		r := Evaluate(context.Background(), evalCheck())
		if !r.OK || r.TeammateMode != "auto" || r.TeammateModeSource != SourceCLI || !reflect.DeepEqual(r.Warnings, []string{WarnAutoMayFallback}) {
			t.Fatalf("got %+v", r)
		}
	})
}

func TestEvaluateReservedProvider(t *testing.T) {
	f := evalSetup(t)
	// the reserved name is refused before providers.toml is read
	evalWriteFile(t, filepath.Join(f.h.ConfigHome, "cc-fleet", "providers.toml"), "not toml [\n")
	r := Evaluate(context.Background(), Input{Provider: config.ReservedNativeProvider, Slot: SlotDefault})
	evalAssertFailure(t, r, CodeBadArgs, "")
	if !strings.Contains(r.ErrorMsg, "reserved") {
		t.Errorf("error_msg %q does not say reserved", r.ErrorMsg)
	}

	for _, typ := range []string{"ccf-claude", "ccf-claude.strong"} {
		var out strings.Builder
		in := `{"tool_name":"Agent","tool_input":{"name":"w","subagent_type":"` + typ + `","prompt":"x"}}`
		if rc := Guard(context.Background(), Protocol, strings.NewReader(in), &out); rc != 0 {
			t.Fatalf("%s: guard exit %d", typ, rc)
		}
		reason := guardReason(t, out.String())
		if !strings.HasPrefix(reason, "cc-fleet: BAD_ARGS: ") || !strings.Contains(reason, "reserved") {
			t.Errorf("%s: reason %q", typ, reason)
		}
	}
}
