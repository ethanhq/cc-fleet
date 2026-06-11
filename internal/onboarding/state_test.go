package onboarding

import (
	"os"
	"path/filepath"
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
	if st.TmuxAck || st.AgentTeamsAck {
		t.Fatalf("missing file should yield zero State (no acks), got %+v", st)
	}
}

func TestState_SaveLoadRoundTrip(t *testing.T) {
	setupHome(t)
	in := State{TmuxAck: true, AgentTeamsAck: true}
	if err := in.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File must be 0600.
	path, _ := StatePath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}

	out, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !out.TmuxAck || !out.AgentTeamsAck {
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
	if st.TmuxAck || st.AgentTeamsAck {
		t.Fatal("corrupt file must yield zero State (no acks) so we re-guide")
	}
}
