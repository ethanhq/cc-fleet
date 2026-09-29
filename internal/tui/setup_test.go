package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/teammate"
)

// TestMain gives the whole package a hermetic baseline: a throwaway HOME with
// onboarding already acked and a stub `claude` on PATH, so NewModel() opens
// straight on the hub rather than a first-run nudge screen. Without it the model
// tests would read the real user's HOME and depend on whether provider
// teammates are set up / claude is installed there. Per-test envs (setupEnv)
// inherit this PATH; the install-Claude tests override it to hide claude.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "cc-fleet-tui")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		panic(err)
	}
	stub := "claude"
	if runtime.GOOS == "windows" {
		stub = "claude.exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, stub), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		panic(err)
	}
	os.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := (onboarding.State{AgentTeamsAck: true, TeammateLaneAck: true}).Save(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// setupEnv installs a hermetic HOME (+USERPROFILE for windows) + XDG + clean
// CWD, points Claude Code's config dir at <home>/.claude, and seeds one enabled
// provider so the provider-teammates nudge applies.
func setupEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE; keep the sandbox hermetic there
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	t.Chdir(t.TempDir())
	tuiSeedProvider(t)
	return home
}

// tuiSeedProvider saves providers.toml with one enabled provider.
func tuiSeedProvider(t *testing.T) {
	t.Helper()
	cfg := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{
		"glm": {
			Name: "glm", BaseURL: "https://glm.example.test/api/anthropic", ModelsEndpoint: "https://glm.example.test/v1/models",
			DefaultModel: "glm-4.6", SecretBackend: "file", SecretRef: "glm", Enabled: true,
		},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("save providers.toml: %v", err)
	}
}

// tuiReadSettings parses <home>/.claude/settings.json.
func tuiReadSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	return parsed
}

func TestNewModel_SetupGating(t *testing.T) {
	setupEnv(t)
	if runtime.GOOS == "windows" {
		// the teammate lane is unix-only, so windows never nudges — a fresh
		// install opens straight on the hub.
		if got := NewModel().screen; got != screenList {
			t.Fatalf("NewModel screen = %d, want screenList on windows", got)
		}
		return
	}
	// Lane off + unacked → open on the provider-teammates setup screen.
	if got := NewModel().screen; got != screenSetup {
		t.Fatalf("NewModel screen = %d, want screenSetup", got)
	}
	// After ack → straight to the hub.
	if err := (onboarding.State{TeammateLaneAck: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if got := NewModel().screen; got != screenList {
		t.Fatalf("NewModel screen = %d, want screenList after ack", got)
	}
	// Lane already enabled → hub, even without ack.
	if err := (onboarding.State{TeammateLane: onboarding.TeammateLane{Enabled: true}}).Save(); err != nil {
		t.Fatal(err)
	}
	if got := NewModel().screen; got != screenList {
		t.Fatalf("NewModel screen = %d, want screenList when the lane is enabled", got)
	}
}

func TestUpdateSetup_Navigate(t *testing.T) {
	setupEnv(t)
	m := Model{screen: screenSetup}
	m, _ = press(t, m, "down")
	if m.setupCursor != 1 {
		t.Fatalf("after down: cursor=%d, want 1", m.setupCursor)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // clamp at last option
	if m.setupCursor != setupOptionCount-1 {
		t.Fatalf("cursor=%d, want clamp at %d", m.setupCursor, setupOptionCount-1)
	}
	m, _ = press(t, m, "up")
	if m.setupCursor != 1 {
		t.Fatalf("after up: cursor=%d, want 1", m.setupCursor)
	}
}

func TestUpdateSetup_EnableWritesSettingsAndAcks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	home := setupEnv(t)
	m := Model{screen: screenSetup} // cursor 0 = "enable it for me"
	m, _ = press(t, m, "enter")

	if !strings.Contains(m.setupMsg, "restart claude") {
		t.Fatalf("setupMsg = %q, want a restart hint", m.setupMsg)
	}
	// Parse the JSON and assert the var is a properly-placed, enabled env entry —
	// a raw substring match would pass even if the name appeared malformed or
	// disabled. The on-disk shape is {"env": {"<VAR>": "1"}}.
	env, _ := tuiReadSettings(t, home)["env"].(map[string]any)
	if got := env[teammate.EnvAgentTeams]; got != "1" {
		t.Fatalf("settings.json env[%s] = %v, want %q (enabled)", teammate.EnvAgentTeams, got, "1")
	}
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck || !st.TeammateLaneAck || !st.TeammateLane.Enabled {
		t.Fatalf("state after enable = %+v, want acked and the lane enabled", st)
	}
	// Any key dismisses the note → hub.
	m, _ = press(t, m, "enter")
	if m.screen != screenList {
		t.Fatalf("after note dismiss: screen=%d, want screenList", m.screen)
	}
}

// TestOnboardingEnableWritesTmuxMode: "enable it for me" on a machine without
// settings.json writes teammateMode tmux and points the launcher setting at the
// shim; a user who already chose auto keeps auto.
func TestOnboardingEnableWritesTmuxMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the teammate lane is unix-only")
	}
	for _, tc := range []struct {
		name, preset, want string
	}{
		{"missing settings", "", "tmux"},
		{"auto kept", "auto", "auto"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setupEnv(t)
			if tc.preset != "" {
				if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
					t.Fatal(err)
				}
				data := []byte(`{"teammateMode":"` + tc.preset + `"}`)
				if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			shim, err := teammate.ShimPath()
			if err != nil {
				t.Fatal(err)
			}

			m, _ := press(t, Model{screen: screenSetup}, "enter") // cursor 0 = "enable it for me"
			if !strings.Contains(m.setupMsg, "provider teammates enabled") {
				t.Fatalf("setupMsg = %q, want the enabled note", m.setupMsg)
			}
			settings := tuiReadSettings(t, home)
			if got := settings[teammate.KeyTeammateMode]; got != tc.want {
				t.Errorf("teammateMode = %v, want %q", got, tc.want)
			}
			env, _ := settings["env"].(map[string]any)
			if got := env[teammate.EnvTeammateCommand]; got != shim {
				t.Errorf("env.%s = %v, want the shim %q", teammate.EnvTeammateCommand, got, shim)
			}
		})
	}
}

func TestUpdateSetup_AlreadySetUp_AcksNoWrite(t *testing.T) {
	home := setupEnv(t)
	m := Model{screen: screenSetup, setupCursor: 1} // "I've set it up myself"
	m, _ = press(t, m, "enter")
	if m.screen != screenList {
		t.Fatalf("screen=%d, want screenList", m.screen)
	}
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck || !st.TeammateLaneAck {
		t.Fatal("ack not recorded")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings.json written despite not choosing enable")
	}
}

func TestUpdateSetup_EscDismissesAndAcks(t *testing.T) {
	setupEnv(t)
	m := Model{screen: screenSetup}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("screen=%d, want screenList", m.screen)
	}
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck || !st.TeammateLaneAck {
		t.Fatal("ack not recorded on esc dismiss")
	}
}

// TestSetupView_Wording locks the setup screen's key wording: title, footer,
// the skip option naming every lane that works without provider teammates, and
// the note that enabling sets tmux split panes.
func TestSetupView_Wording(t *testing.T) {
	atView := Model{screen: screenSetup}.viewSetup()
	for _, want := range []string{"cc-fleet · setup", "↑/↓ move · enter select", "skip — I'll only use subagent / workflow / run", "tmux split panes"} {
		if !strings.Contains(atView, want) {
			t.Errorf("provider-teammates setup view missing %q", want)
		}
	}
}
