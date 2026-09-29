package teammate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
)

// cliLaneHome is hermeticHome for tests of the teammate lane itself, which
// Setup refuses on Windows.
func cliLaneHome(t *testing.T) testHome {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	return hermeticHome(t)
}

// cliSeedProviders saves providers.toml with one enabled provider "glm"
// (strong differs from default, so it gets a slot definition) and a disabled one.
func cliSeedProviders(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{
		"glm": {
			Name: "glm", BaseURL: "https://glm.example.test/api/anthropic", ModelsEndpoint: "https://glm.example.test/v1/models",
			DefaultModel: "glm-4.6", StrongModel: "glm-5", SecretBackend: "file", SecretRef: "glm", Enabled: true,
		},
		"off": {
			Name: "off", BaseURL: "https://off.example.test/api/anthropic", ModelsEndpoint: "https://off.example.test/v1/models",
			DefaultModel: "off-1", SecretBackend: "file", SecretRef: "off", Enabled: false,
		},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save providers.toml: %v", err)
	}
	return cfg
}

// cliWriteSettings writes v as the user settings.json.
func cliWriteSettings(t *testing.T, h testHome, v any) string {
	t.Helper()
	p := filepath.Join(h.ClaudeDir, "settings.json")
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// cliSetting reads a string key path from the user settings.json.
func cliSetting(t *testing.T, h testHome, keyPath ...string) (string, bool) {
	t.Helper()
	v, ok, err := onboarding.SettingsString(filepath.Join(h.ClaudeDir, "settings.json"), keyPath...)
	if err != nil {
		t.Fatalf("read settings %v: %v", keyPath, err)
	}
	return v, ok
}

func cliState(t *testing.T) onboarding.State {
	t.Helper()
	st, err := onboarding.LoadState()
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

func cliMustOK(t *testing.T, res SetupResult) {
	t.Helper()
	if !res.OK {
		t.Fatalf("Setup failed: %s(%s): %s", res.ErrorCode, res.Detail, res.ErrorMsg)
	}
}

func TestSetupFresh(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)

	res := Setup(SetupOptions{TeammateMode: "keep"})
	cliMustOK(t, res)
	shim, _ := ShimPath()
	if res.Action != "setup" || res.Shim != shim || res.SettingsPath != filepath.Join(h.ClaudeDir, "settings.json") {
		t.Errorf("result = %+v", res)
	}
	wantChanged := []string{"env." + EnvTeammateCommand, "env." + EnvAgentTeams}
	if !reflect.DeepEqual(res.Changed, wantChanged) {
		t.Errorf("changed = %v, want %v", res.Changed, wantChanged)
	}
	if res.AgentDefs == nil || !reflect.DeepEqual(res.AgentDefs.Written, []string{"ccf-glm.md", "ccf-glm.strong.md"}) {
		t.Errorf("agent_defs = %+v", res.AgentDefs)
	}
	if !res.RestartRequired || res.ModeWritten || res.TeammateMode != "in-process" || res.TeammateModeSource != SourceDefault {
		t.Errorf("restart=%v mode_written=%v mode=%q source=%q", res.RestartRequired, res.ModeWritten, res.TeammateMode, res.TeammateModeSource)
	}
	if res.Warnings == nil || len(res.Warnings) != 0 {
		t.Errorf("warnings = %#v, want []", res.Warnings)
	}

	if st := InspectShim(shim); !st.Exists || !st.Executable || !st.Managed || !st.PinnedIsSelf {
		t.Errorf("shim = %+v", st)
	}
	if v, _ := cliSetting(t, h, "env", EnvTeammateCommand); v != shim {
		t.Errorf("settings %s = %q, want %q", EnvTeammateCommand, v, shim)
	}
	if v, _ := cliSetting(t, h, "env", EnvAgentTeams); v != "1" {
		t.Errorf("settings %s = %q, want 1", EnvAgentTeams, v)
	}
	if _, ok := cliSetting(t, h, KeyTeammateMode); ok {
		t.Error("keep must not write teammateMode")
	}
	if !IsManagedDef(filepath.Join(h.ClaudeDir, "agents", "ccf-glm.md")) {
		t.Error("ccf-glm.md not written")
	}
	if st := cliState(t); !st.TeammateLane.Enabled || st.TeammateLane.ModeWrittenAt != 0 {
		t.Errorf("lane state = %+v", st.TeammateLane)
	}

	// The setup envelope's JSON shape.
	data, _ := json.Marshal(res)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	for _, k := range []string{"ok", "action", "shim", "settings_path", "changed", "agent_defs", "teammate_mode", "teammate_mode_source", "mode_written", "restart_required", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("JSON lacks %q: %s", k, data)
		}
	}
}

func TestSetupIdempotent(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)
	cliMustOK(t, Setup(SetupOptions{TeammateMode: "tmux"}))
	settings := filepath.Join(h.ClaudeDir, "settings.json")
	before, _ := os.ReadFile(settings)
	stBefore := cliState(t)

	res := Setup(SetupOptions{TeammateMode: "tmux"})
	cliMustOK(t, res)
	if len(res.Changed) != 0 || res.RestartRequired || res.ModeWritten {
		t.Errorf("second run changed=%v restart=%v mode_written=%v", res.Changed, res.RestartRequired, res.ModeWritten)
	}
	if len(res.AgentDefs.Written) != 0 || len(res.AgentDefs.Unchanged) != 2 {
		t.Errorf("second run agent_defs = %+v", res.AgentDefs)
	}
	if res.TeammateMode != "tmux" || res.TeammateModeSource != SourceUser {
		t.Errorf("mode = %q (%s)", res.TeammateMode, res.TeammateModeSource)
	}
	after, _ := os.ReadFile(settings)
	if string(before) != string(after) {
		t.Errorf("settings rewritten:\n%s\n---\n%s", before, after)
	}
	if st := cliState(t); st.TeammateLane != stBefore.TeammateLane {
		t.Errorf("lane state %+v → %+v", stBefore.TeammateLane, st.TeammateLane)
	}
}

func TestSetupConflictForeignLauncher(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)
	foreign := writeScript(t, filepath.Join(h.Home, "other-launcher"), "exit 0\n")
	cliWriteSettings(t, h, map[string]any{"env": map[string]any{EnvTeammateCommand: foreign}})
	settings := filepath.Join(h.ClaudeDir, "settings.json")
	before, _ := os.ReadFile(settings)

	res := Setup(SetupOptions{TeammateMode: "keep"})
	if res.OK || res.ErrorCode != CodeSetupConflict || res.Detail != DetailLauncherForeign {
		t.Fatalf("without --force: %+v", res)
	}
	after, _ := os.ReadFile(settings)
	if string(before) != string(after) {
		t.Error("settings changed on conflict")
	}
	shim, _ := ShimPath()
	if _, err := os.Stat(shim); !os.IsNotExist(err) {
		t.Errorf("shim written on conflict (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.ClaudeDir, "agents")); !os.IsNotExist(err) {
		t.Errorf("agents dir created on conflict (err=%v)", err)
	}
	if cliState(t).TeammateLane.Enabled {
		t.Error("lane enabled on conflict")
	}

	res = Setup(SetupOptions{TeammateMode: "keep", Force: true})
	cliMustOK(t, res)
	if v, _ := cliSetting(t, h, "env", EnvTeammateCommand); v != shim {
		t.Errorf("with --force: %s = %q, want %q", EnvTeammateCommand, v, shim)
	}
}

func TestSetupConflictForeignDef(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)
	foreign := filepath.Join(h.ClaudeDir, "agents", "ccf-glm.strong.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("---\nname: ccf-glm.strong\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, force := range []bool{false, true} {
		res := Setup(SetupOptions{TeammateMode: "keep", Force: force})
		if res.OK || res.ErrorCode != CodeSetupConflict || res.Detail != DetailAgentDefForeign {
			t.Fatalf("force=%v: %+v", force, res)
		}
	}
	if data, _ := os.ReadFile(foreign); string(data) != "---\nname: ccf-glm.strong\n---\nmine\n" {
		t.Errorf("foreign definition touched: %q", data)
	}
	if _, err := os.Stat(filepath.Join(h.ClaudeDir, "agents", "ccf-glm.md")); !os.IsNotExist(err) {
		t.Errorf("a definition was written despite the conflict (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(h.ClaudeDir, "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings written despite the conflict (err=%v)", err)
	}
}

func TestSetupModeKeepVsTmux(t *testing.T) {
	cases := []struct {
		name       string
		user       any // teammateMode at the user layer; nil = absent
		mode       string
		wantMode   string
		wantSource string
		written    bool
	}{
		{"keep/unset", nil, "keep", "in-process", SourceDefault, false},
		{"keep/in-process", "in-process", "keep", "in-process", SourceUser, false},
		{"default-is-keep/unset", nil, "", "in-process", SourceDefault, false},
		{"tmux/unset", nil, "tmux", "tmux", SourceUser, true},
		{"tmux/in-process", "in-process", "tmux", "tmux", SourceUser, true},
		{"tmux/auto", "auto", "tmux", "auto", SourceUser, false},
		{"tmux/iterm2", "iterm2", "tmux", "iterm2", SourceUser, false},
		{"tmux/tmux", "tmux", "tmux", "tmux", SourceUser, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := cliLaneHome(t)
			cliSeedProviders(t)
			settings := map[string]any{"env": map[string]any{"KEEP": "&<>"}}
			if tc.user != nil {
				settings[KeyTeammateMode] = tc.user
			}
			cliWriteSettings(t, h, settings)

			res := Setup(SetupOptions{TeammateMode: tc.mode})
			cliMustOK(t, res)
			if res.ModeWritten != tc.written || slices.Contains(res.Changed, KeyTeammateMode) != tc.written {
				t.Errorf("mode_written=%v changed=%v, want written=%v", res.ModeWritten, res.Changed, tc.written)
			}
			if res.TeammateMode != tc.wantMode || res.TeammateModeSource != tc.wantSource {
				t.Errorf("mode = %q (%s), want %q (%s)", res.TeammateMode, res.TeammateModeSource, tc.wantMode, tc.wantSource)
			}
			got, present := cliSetting(t, h, KeyTeammateMode)
			switch {
			case tc.written && got != "tmux":
				t.Errorf("settings teammateMode = %q, want tmux", got)
			case !tc.written && tc.user == nil && present:
				t.Errorf("settings teammateMode = %q, want absent", got)
			case !tc.written && tc.user != nil && got != tc.user:
				t.Errorf("settings teammateMode = %q, want %v", got, tc.user)
			}
			if v, _ := cliSetting(t, h, "env", "KEEP"); v != "&<>" {
				t.Errorf("unrelated key = %q", v)
			}
			st := cliState(t)
			if (st.TeammateLane.ModeWrittenAt != 0) != tc.written {
				t.Errorf("mode_written_at = %d, want set=%v", st.TeammateLane.ModeWrittenAt, tc.written)
			}
			if tc.written && !res.RestartRequired {
				t.Error("writing teammateMode must require a restart")
			}
		})
	}

	t.Run("bad mode", func(t *testing.T) {
		cliLaneHome(t)
		cliSeedProviders(t)
		if res := Setup(SetupOptions{TeammateMode: "auto"}); res.OK || res.ErrorCode != CodeBadArgs {
			t.Errorf("mode auto: %+v", res)
		}
	})
}

func TestSetupRemove(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)
	cliWriteSettings(t, h, map[string]any{"teammateMode": "auto"})
	cliMustOK(t, Setup(SetupOptions{TeammateMode: "tmux"}))
	shim, _ := ShimPath()
	unmarked := filepath.Join(h.ClaudeDir, "agents", "ccf-mine.md")
	if err := os.WriteFile(unmarked, []byte("---\nname: ccf-mine\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := Setup(SetupOptions{Remove: true})
	cliMustOK(t, res)
	if res.Action != "remove" || !reflect.DeepEqual(res.Changed, []string{"env." + EnvTeammateCommand}) || !res.RestartRequired {
		t.Errorf("result = %+v", res)
	}
	if res.AgentDefs == nil || !reflect.DeepEqual(res.AgentDefs.Removed, []string{"ccf-glm.md", "ccf-glm.strong.md"}) {
		t.Errorf("agent_defs = %+v", res.AgentDefs)
	}
	if _, ok := cliSetting(t, h, "env", EnvTeammateCommand); ok {
		t.Error("launcher setting still present")
	}
	if v, _ := cliSetting(t, h, "env", EnvAgentTeams); v != "1" {
		t.Errorf("%s = %q, want kept", EnvAgentTeams, v)
	}
	if v, _ := cliSetting(t, h, KeyTeammateMode); v != "auto" {
		t.Errorf("teammateMode = %q, want kept", v)
	}
	if _, err := os.Stat(shim); err != nil {
		t.Errorf("shim must be kept: %v", err)
	}
	if _, err := os.Stat(unmarked); err != nil {
		t.Errorf("unmarked definition removed: %v", err)
	}
	if cliState(t).TeammateLane.Enabled {
		t.Error("lane still enabled")
	}

	// A foreign launcher is not ours to delete; a second remove changes nothing.
	cliWriteSettings(t, h, map[string]any{"env": map[string]any{EnvTeammateCommand: "/opt/other"}})
	res = Setup(SetupOptions{Remove: true})
	cliMustOK(t, res)
	if len(res.Changed) != 0 || res.RestartRequired {
		t.Errorf("foreign remove: %+v", res)
	}
	if v, _ := cliSetting(t, h, "env", EnvTeammateCommand); v != "/opt/other" {
		t.Errorf("foreign launcher = %q, want kept", v)
	}
}

func TestSetupSettingsUnparseable(t *testing.T) {
	h := cliLaneHome(t)
	cliSeedProviders(t)
	for _, body := range []string{"[1,2]", "{not json", `{"env":"x"}`} {
		if err := os.WriteFile(filepath.Join(h.ClaudeDir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, remove := range []bool{false, true} {
			if res := Setup(SetupOptions{Remove: remove}); res.OK || res.ErrorCode != CodeSettingsUnparseable {
				t.Errorf("%q remove=%v: %+v", body, remove, res)
			}
		}
	}
}

func TestNeedsSetupNudge(t *testing.T) {
	cliLaneHome(t)
	if NeedsSetupNudge() {
		t.Error("no providers: want false")
	}
	cliSeedProviders(t)
	if !NeedsSetupNudge() {
		t.Error("enabled provider, lane off, no ack: want true")
	}

	st := cliState(t)
	st.TeammateLaneAck = true
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if NeedsSetupNudge() {
		t.Error("acked: want false")
	}

	st.TeammateLaneAck = false
	st.TeammateLane.Enabled = true
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if NeedsSetupNudge() {
		t.Error("lane enabled: want false")
	}

	st.TeammateLane.Enabled = false
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	cfg.Providers["glm"].Enabled = false
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if NeedsSetupNudge() {
		t.Error("only disabled providers: want false")
	}
}

func TestAgentDefsDrift(t *testing.T) {
	h := hermeticHome(t)
	cfg := cliSeedProviders(t)
	stale, foreign := AgentDefsDrift(cfg)
	if !reflect.DeepEqual(stale, []string{"ccf-glm.md", "ccf-glm.strong.md"}) || len(foreign) != 0 {
		t.Fatalf("empty dir: stale=%v foreign=%v", stale, foreign)
	}
	if _, err := SyncAgentDefs(cfg); err != nil {
		t.Fatal(err)
	}
	if stale, foreign := AgentDefsDrift(cfg); len(stale)+len(foreign) != 0 {
		t.Fatalf("after sync: stale=%v foreign=%v", stale, foreign)
	}
	if err := os.WriteFile(filepath.Join(h.ClaudeDir, "agents", "ccf-x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.Providers["glm"].StrongModel = ""
	stale, foreign = AgentDefsDrift(cfg)
	if !reflect.DeepEqual(stale, []string{"ccf-glm.strong.md"}) || !reflect.DeepEqual(foreign, []string{"ccf-x.md"}) {
		t.Errorf("drift: stale=%v foreign=%v", stale, foreign)
	}
}
