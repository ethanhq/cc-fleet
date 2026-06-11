package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/onboarding"
)

// TestMain gives the whole package a hermetic baseline: a throwaway HOME with
// onboarding already acked, so NewModel() opens straight on the hub rather than
// a first-run setup screen. Without it the model tests would read the real
// user's HOME and pass or fail depending on whether agent-teams happens to be
// configured there. The onboarding tests override HOME per-test via setupEnv(t)
// to exercise the nudges.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "cc-fleet-tui")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err := (onboarding.State{AgentTeamsAck: true}).Save(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// setupEnv installs a hermetic HOME (+USERPROFILE for windows) + XDG + clean
// CWD and clears any inherited agent-teams env var.
func setupEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE; keep the sandbox hermetic there
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	t.Chdir(t.TempDir())
	return home
}

func TestNewModel_SetupGating(t *testing.T) {
	setupEnv(t)
	if runtime.GOOS == "windows" {
		// agent-teams powers the unix-only teammate lane, so windows never
		// nudges — a fresh install opens straight on the hub.
		if got := NewModel().screen; got != screenList {
			t.Fatalf("NewModel screen = %d, want screenList on windows", got)
		}
		return
	}
	// Unconfigured + unacked → open on the agent-teams setup screen.
	if got := NewModel().screen; got != screenSetup {
		t.Fatalf("NewModel screen = %d, want screenSetup", got)
	}
	// After ack → straight to the hub.
	if err := (onboarding.State{AgentTeamsAck: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if got := NewModel().screen; got != screenList {
		t.Fatalf("NewModel screen = %d, want screenList after ack", got)
	}
	// Configured (env set) → hub, even without ack.
	if err := (onboarding.State{}).Save(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "1")
	if got := NewModel().screen; got != screenList {
		t.Fatalf("NewModel screen = %d, want screenList when configured", got)
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
	home := setupEnv(t)
	m := Model{screen: screenSetup} // cursor 0 = "enable it for me"
	m, _ = press(t, m, "enter")

	if !strings.Contains(m.setupMsg, "restart claude") {
		t.Fatalf("setupMsg = %q, want a restart hint", m.setupMsg)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("settings.json not written: %v", err)
	}
	// Parse the JSON and assert the var is a properly-placed, enabled env entry —
	// a raw substring match would pass even if the name appeared malformed or
	// disabled. The on-disk shape is {"env": {"<VAR>": "1"}}.
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v\n%s", err, data)
	}
	if got := parsed.Env[onboarding.AgentTeamsEnvVar]; got != "1" {
		t.Fatalf("settings.json env[%s] = %q, want %q (enabled)",
			onboarding.AgentTeamsEnvVar, got, "1")
	}
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck {
		t.Fatal("ack not recorded after enable")
	}
	// Any key dismisses the note → hub.
	m, _ = press(t, m, "enter")
	if m.screen != screenList {
		t.Fatalf("after note dismiss: screen=%d, want screenList", m.screen)
	}
}

func TestUpdateSetup_AlreadySetUp_AcksNoWrite(t *testing.T) {
	home := setupEnv(t)
	m := Model{screen: screenSetup, setupCursor: 1} // "I've set it up myself"
	m, _ = press(t, m, "enter")
	if m.screen != screenList {
		t.Fatalf("screen=%d, want screenList", m.screen)
	}
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck {
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
	if st, _ := onboarding.LoadState(); !st.AgentTeamsAck {
		t.Fatal("ack not recorded on esc dismiss")
	}
}

// TestSetupView_Wording locks the setup screen's key wording: title, footer,
// and the skip option naming every lane that works without agent-teams.
func TestSetupView_Wording(t *testing.T) {
	atView := Model{screen: screenSetup}.viewSetup()
	for _, want := range []string{"cc-fleet · setup", "↑/↓ move · enter select", "skip — I'll only use subagent / workflow / run"} {
		if !strings.Contains(atView, want) {
			t.Errorf("agent-teams setup view missing %q", want)
		}
	}
}
