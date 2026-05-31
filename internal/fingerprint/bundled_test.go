package fingerprint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBundledFingerprint_ParsesRecipe locks the embedded default: it must
// parse, carry the two teammate env vars + the flag template, be tagged with
// BundledVersion, and (critically) NOT pin a binary path — the path is
// resolved live at spawn.
func TestBundledFingerprint_ParsesRecipe(t *testing.T) {
	fp, err := bundledFingerprint()
	if err != nil {
		t.Fatalf("bundledFingerprint: %v", err)
	}
	if fp.CCVersion != BundledVersion {
		t.Fatalf("bundled cc_version = %q, want BundledVersion %q", fp.CCVersion, BundledVersion)
	}
	if fp.BinaryPath != "" {
		t.Fatalf("bundled binary_path must be empty (resolved dynamically), got %q", fp.BinaryPath)
	}
	if fp.Env["CLAUDECODE"] != "1" || fp.Env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"] != "1" {
		t.Fatalf("bundled env missing teammate vars: %+v", fp.Env)
	}
	if len(fp.FlagsTemplate) == 0 {
		t.Fatal("bundled flags_template is empty")
	}
	// The placeholder flags the spawn template depends on must all be present.
	joined := ""
	for _, f := range fp.FlagsTemplate {
		joined += f + " "
	}
	for _, want := range []string{"{name}@{team}", "{team}", "{color}", "{lead_session_id}"} {
		if !contains(joined, want) {
			t.Fatalf("bundled flags_template missing placeholder %q: %v", want, fp.FlagsTemplate)
		}
	}
}

// TestLoadOrBundled_FallsBackToBundled: with no fingerprint.json on disk,
// LoadOrBundled returns the bundled recipe instead of ErrNotFound — the
// no-mandatory-probe guarantee.
func TestLoadOrBundled_FallsBackToBundled(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	fp, err := LoadOrBundled()
	if err != nil {
		t.Fatalf("LoadOrBundled with no cache: %v", err)
	}
	if fp.CCVersion != BundledVersion {
		t.Fatalf("expected bundled recipe (cc %s), got cc %s", BundledVersion, fp.CCVersion)
	}
}

// TestLoadOrBundled_PrefersUserCache: a present fingerprint.json wins over the
// bundled default (a probe-written recipe overrides the shipped one).
func TestLoadOrBundled_PrefersUserCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	user := &Fingerprint{
		CCVersion:     "9.9.9",
		BinaryPath:    "/some/where/claude",
		Env:           map[string]string{"CLAUDECODE": "1"},
		FlagsTemplate: []string{"--agent-id", "{name}@{team}"},
	}
	if err := Save(user); err != nil {
		t.Fatalf("Save: %v", err)
	}

	fp, err := LoadOrBundled()
	if err != nil {
		t.Fatalf("LoadOrBundled: %v", err)
	}
	if fp.CCVersion != "9.9.9" {
		t.Fatalf("expected user cache (cc 9.9.9), got cc %s", fp.CCVersion)
	}
}

// TestLoadOrBundled_SurfacesCorruptCache: a present-but-unparseable file is a
// real error, NOT silently masked by the bundled fallback.
func TestLoadOrBundled_SurfacesCorruptCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrBundled(); err == nil {
		t.Fatal("LoadOrBundled must surface a corrupt existing cache, got nil error")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return len(needle) == 0
}
