package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// cleanAgentTeamsEnv installs a hermetic HOME + XDG + clean CWD and CLEARS any
// inherited CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS (the dev shell may export it),
// so detection starts from a known-empty state.
func cleanAgentTeamsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(AgentTeamsEnvVar, "")
	t.Chdir(t.TempDir()) // clean CWD so the project-level probe sees nothing
	return home
}

func TestEnvTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", ` "1" `, "'1'"} {
		if !envTruthy(v) {
			t.Errorf("envTruthy(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "garbage"} {
		if envTruthy(v) {
			t.Errorf("envTruthy(%q) = true, want false", v)
		}
	}
}

func TestAgentTeamsConfigured_None(t *testing.T) {
	cleanAgentTeamsEnv(t)
	if AgentTeamsConfigured() {
		t.Fatal("want false when no source sets the var")
	}
}

func TestAgentTeamsConfigured_Env(t *testing.T) {
	cleanAgentTeamsEnv(t)
	t.Setenv(AgentTeamsEnvVar, "1")
	if !AgentTeamsConfigured() {
		t.Fatal("want true when current env sets the var")
	}
}

func TestAgentTeamsConfigured_Zshrc(t *testing.T) {
	home := cleanAgentTeamsEnv(t)
	rc := "# my shell\nexport CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	if !AgentTeamsConfigured() {
		t.Fatal("want true when ~/.zshrc exports the var")
	}
}

func TestAgentTeamsConfigured_GlobalSettings(t *testing.T) {
	home := cleanAgentTeamsEnv(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"opus","env":{"FOO":"bar","CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS":"1"}}`
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AgentTeamsConfigured() {
		t.Fatal("want true when global settings.json env sets the var")
	}
}

func TestAgentTeamsConfigured_ProjectSettings(t *testing.T) {
	cleanAgentTeamsEnv(t)
	if err := os.MkdirAll(".claude", 0o700); err != nil { // in the clean CWD
		t.Fatal(err)
	}
	body := `{"env":{"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS":"true"}}`
	if err := os.WriteFile(filepath.Join(".claude", "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if !AgentTeamsConfigured() {
		t.Fatal("want true when project .claude/settings.json sets the var")
	}
}

func TestNeedsAgentTeamsSetup(t *testing.T) {
	cleanAgentTeamsEnv(t)
	if runtime.GOOS == "windows" {
		if NeedsAgentTeamsSetup() {
			t.Fatal("the teammate-lane nudge must never show on windows")
		}
		return
	}
	// Unconfigured + never acked → show the nudge.
	if !NeedsAgentTeamsSetup() {
		t.Fatal("want true: unconfigured + unacked")
	}
	// After ack → never again.
	if err := (State{AgentTeamsAck: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if NeedsAgentTeamsSetup() {
		t.Fatal("want false after ack")
	}
	// Configured short-circuits regardless of ack.
	if err := (State{}).Save(); err != nil { // ack back to false
		t.Fatal(err)
	}
	t.Setenv(AgentTeamsEnvVar, "1")
	if NeedsAgentTeamsSetup() {
		t.Fatal("want false when configured (even with ack=false)")
	}
}

// ---------- EnableAgentTeams (the "enable it for me" action) ----------

func readSettingsEnvVar(t *testing.T) string {
	t.Helper()
	path, _ := settingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse settings.json: %v\n%s", err, data)
	}
	env, _ := m["env"].(map[string]any)
	s, _ := env[AgentTeamsEnvVar].(string)
	return s
}

func TestEnableAgentTeams_CreatesAndIdempotent(t *testing.T) {
	home := cleanAgentTeamsEnv(t)
	already, err := EnableAgentTeams()
	if err != nil || already {
		t.Fatalf("first call: already=%v err=%v, want false,nil", already, err)
	}
	if v := readSettingsEnvVar(t); v != "1" {
		t.Fatalf("env var = %q, want 1", v)
	}
	// mode 0600
	info, _ := os.Stat(filepath.Join(home, ".claude", "settings.json"))
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 { // no unix mode bits on windows
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
	// second call → already, bytes unchanged
	path, _ := settingsPath()
	first, _ := os.ReadFile(path)
	already, err = EnableAgentTeams()
	if err != nil || !already {
		t.Fatalf("second call: already=%v err=%v, want true,nil", already, err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatalf("idempotent call changed bytes:\n%s\n%s", first, second)
	}
}

func TestEnableAgentTeams_PreservesOtherKeys(t *testing.T) {
	home := cleanAgentTeamsEnv(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	orig := `{"model":"opus","permissions":{"allow":["Bash"]},"env":{"FOO":"bar"}}`
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableAgentTeams(); err != nil {
		t.Fatal(err)
	}
	path, _ := settingsPath()
	data, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "opus" {
		t.Errorf("model clobbered: %v", m["model"])
	}
	if _, ok := m["permissions"].(map[string]any); !ok {
		t.Errorf("permissions clobbered: %v", m["permissions"])
	}
	env := m["env"].(map[string]any)
	if env["FOO"] != "bar" || env[AgentTeamsEnvVar] != "1" {
		t.Errorf("env wrong: %v", env)
	}
}

func TestEnableAgentTeams_RejectsNonObjectRoot(t *testing.T) {
	home := cleanAgentTeamsEnv(t)
	cdir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "settings.json"), []byte(`["nope"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnableAgentTeams(); err == nil {
		t.Fatal("want error for non-object settings.json (must not clobber)")
	}
	data, _ := os.ReadFile(filepath.Join(cdir, "settings.json"))
	if string(data) != `["nope"]` {
		t.Fatalf("file modified despite error: %s", data)
	}
}
