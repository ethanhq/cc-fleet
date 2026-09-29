package onboarding

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupHome installs a hermetic $HOME + $XDG_CONFIG_HOME so StatePath /
// settingsPath resolve under a t.TempDir().
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE; keep the sandbox hermetic there
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestLoadState_MissingIsZero(t *testing.T) {
	setupHome(t)
	st, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState on missing file: unexpected error %v", err)
	}
	if st.AgentTeamsAck {
		t.Fatalf("missing file should yield zero State (no ack), got %+v", st)
	}
}

func TestState_SaveLoadRoundTrip(t *testing.T) {
	setupHome(t)
	in := State{AgentTeamsAck: true}
	if err := in.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File must be 0600.
	path, _ := StatePath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 { // no unix mode bits on windows
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}

	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !out.AgentTeamsAck {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if out.Version != stateVersion {
		t.Fatalf("Version = %d, want %d (stamped on Save)", out.Version, stateVersion)
	}
}

func TestLoadState_CorruptTreatedAsZero(t *testing.T) {
	setupHome(t)
	path, _ := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	st, err := LoadState()
	if err == nil {
		t.Fatal("want parse error for corrupt file (caller may log it)")
	}
	if st.AgentTeamsAck {
		t.Fatal("corrupt file must yield zero State (no ack) so we re-guide")
	}
}

func TestStateRoundTripTeammateLane(t *testing.T) {
	setupHome(t)
	in := State{
		AgentTeamsAck:   true,
		TeammateLaneAck: true,
		TeammateLane:    TeammateLane{Enabled: true, ModeWrittenAt: 1790000000123},
	}
	if err := in.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	in.Version = stateVersion
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}

	path, _ := StatePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"teammate_lane_ack":true`, `"teammate_lane":{"enabled":true,"mode_written_at":1790000000123}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("onboarding.json lacks %s: %s", want, raw)
		}
	}

	// A file written before the lane fields existed reads them as zero values.
	if err := os.WriteFile(path, []byte(`{"version":1,"agent_teams_ack":true,"claude_install_ack":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState legacy: %v", err)
	}
	if !old.AgentTeamsAck || !old.ClaudeInstallAck || old.TeammateLaneAck || old.TeammateLane != (TeammateLane{}) {
		t.Fatalf("legacy file = %+v", old)
	}
}
