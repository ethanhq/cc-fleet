package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if err := (onboarding.State{TmuxAck: true, AgentTeamsAck: true}).Save(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// setupEnv installs a hermetic HOME + XDG + clean CWD, clears any inherited
// agent-teams env var, and puts a fake tmux on PATH (so tmux looks INSTALLED by
// default — the common case). Tests that want tmux missing call noTmux(t).
func setupEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "")
	t.Chdir(t.TempDir())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir)
	return home
}

// noTmux makes `tmux -V` fail (empty PATH) so NeedsTmuxSetup triggers.
func noTmux(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestNewModel_SetupGating(t *testing.T) {
	setupEnv(t) // tmux present → skips the tmux screen
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

// ---------- tmux setup screen ----------

func TestNewModel_TmuxGating(t *testing.T) {
	setupEnv(t)
	noTmux(t)                                             // tmux missing → tmux screen
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "1") // agent-teams configured → isolate tmux gating
	if got := NewModel().screen; got != screenSetupTmux {
		t.Fatalf("NewModel screen = %d, want screenSetupTmux", got)
	}
	// TmuxAck + agent-teams configured → straight to the hub.
	if err := (onboarding.State{TmuxAck: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if got := NewModel().screen; got != screenList {
		t.Fatalf("NewModel screen = %d, want screenList after TmuxAck", got)
	}
}

func TestUpdateSetupTmux_Navigate(t *testing.T) {
	setupEnv(t)
	m := Model{screen: screenSetupTmux}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // clamp at last (2 options)
	if m.tmuxCursor != tmuxOptionCount-1 {
		t.Fatalf("cursor=%d, want clamp at %d", m.tmuxCursor, tmuxOptionCount-1)
	}
	m, _ = press(t, m, "up")
	if m.tmuxCursor != 0 {
		t.Fatalf("after up: cursor=%d, want 0", m.tmuxCursor)
	}
}

func TestUpdateSetupTmux_Install_QuitsWithNoteNoAck(t *testing.T) {
	setupEnv(t)
	m := Model{screen: screenSetupTmux} // cursor 0 = "install it"
	m, cmd := press(t, m, "enter")
	if !m.quitting || cmd == nil {
		t.Fatalf("install should quit: quitting=%v cmd=%v", m.quitting, cmd)
	}
	if !strings.Contains(m.postQuitNote, "tmux") || !strings.Contains(m.postQuitNote, onboarding.TmuxInstallHint()) {
		t.Fatalf("postQuitNote = %q, want the install command", m.postQuitNote)
	}
	if st, _ := onboarding.LoadState(); st.TmuxAck {
		t.Fatal("install must NOT set TmuxAck (nudge should return until tmux is present)")
	}
}

func TestUpdateSetupTmux_Skip_AcksThenAgentTeams(t *testing.T) {
	setupEnv(t)                                        // agent-teams env cleared → still needed after tmux skip
	m := Model{screen: screenSetupTmux, tmuxCursor: 1} // "skip — subagent mode only"
	m, _ = press(t, m, "enter")
	if st, _ := onboarding.LoadState(); !st.TmuxAck {
		t.Fatal("skip must set TmuxAck")
	}
	if m.screen != screenSetup {
		t.Fatalf("screen=%d, want screenSetup (agent-teams nudge next)", m.screen)
	}
}

func TestUpdateSetupTmux_Skip_ToHubWhenAgentTeamsConfigured(t *testing.T) {
	setupEnv(t)
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "1") // agent-teams configured
	m := Model{screen: screenSetupTmux, tmuxCursor: 1}
	m, _ = press(t, m, "esc") // esc also skips
	if st, _ := onboarding.LoadState(); !st.TmuxAck {
		t.Fatal("esc skip must set TmuxAck")
	}
	if m.screen != screenList {
		t.Fatalf("screen=%d, want screenList (agent-teams already configured)", m.screen)
	}
}

// TestSetupScreens_Aligned locks the "two nudges read the same" goal: identical
// title + footer, and the same "skip — subagent mode" wording on both.
func TestSetupScreens_Aligned(t *testing.T) {
	tmuxView := Model{screen: screenSetupTmux}.viewSetupTmux()
	atView := Model{screen: screenSetup}.viewSetup()
	for _, want := range []string{"cc-fleet · setup", "↑/↓ move · enter select", "skip — I'll only use subagent mode"} {
		if !strings.Contains(tmuxView, want) {
			t.Errorf("tmux setup view missing %q", want)
		}
		if !strings.Contains(atView, want) {
			t.Errorf("agent-teams setup view missing %q", want)
		}
	}
}
