package fingerprint

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleFingerprint mirrors what a real probe against cc 2.1.150 yields.
func sampleFingerprint() *Fingerprint {
	return &Fingerprint{
		CCVersion:  "2.1.150",
		CapturedAt: time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC),
		BinaryPath: "/root/.local/share/claude/versions/2.1.150",
		Env: map[string]string{
			"CLAUDECODE":                           "1",
			"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS": "1",
		},
		FlagsTemplate: []string{
			"--agent-id", "{name}@{team}",
			"--agent-name", "{name}",
			"--team-name", "{team}",
			"--agent-color", "{color}",
			"--parent-session-id", "{lead_session_id}",
			"--agent-type", "general-purpose",
			"--dangerously-skip-permissions",
		},
	}
}

// isolateConfigDir points XDG_CONFIG_HOME at a fresh temp dir so Path() is
// sandboxed for the test. Returns the cc-fleet config root.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", filepath.Join(xdg, "fakehome"))
	return filepath.Join(xdg, "cc-fleet")
}

func TestPath_UsesConfigDir(t *testing.T) {
	root := isolateConfigDir(t)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(root, "fingerprint.json")
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	isolateConfigDir(t)
	fp := sampleFingerprint()

	if err := Save(fp); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, fp) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, fp)
	}
}

func TestLoad_MissingFile_ReturnsErrNotFound(t *testing.T) {
	isolateConfigDir(t)
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error for missing file, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load err = %v, want wrapped ErrNotFound", err)
	}
}

func TestLoadFromPath_MissingFile_ReturnsErrNotFound(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	_, err := LoadFromPath(missing)
	if err == nil {
		t.Fatalf("LoadFromPath: want error for missing file, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadFromPath err = %v, want wrapped ErrNotFound", err)
	}
}

func TestSave_FilePerm0600(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(sampleFingerprint()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600", got)
	}
}

func TestSave_Atomic_NoTempLeft(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(sampleFingerprint()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leaked temp file: %s", e.Name())
		}
	}
}

func TestSave_OverwriteExisting(t *testing.T) {
	isolateConfigDir(t)
	fp1 := sampleFingerprint()
	if err := Save(fp1); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	fp2 := sampleFingerprint()
	fp2.CCVersion = "2.1.200"
	if err := Save(fp2); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CCVersion != "2.1.200" {
		t.Fatalf("CCVersion = %q after overwrite, want 2.1.200", got.CCVersion)
	}
}

func TestSave_RejectsNil(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(nil); err == nil {
		t.Fatalf("Save(nil): want error, got nil")
	}
}

func TestSave_JSONShape(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(sampleFingerprint()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// The on-disk schema is part of the design contract — make sure all five
	// documented fields are present. We don't pin formatting; we just confirm
	// the JSON parses and exposes the expected top-level keys.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	want := []string{"cc_version", "captured_at", "binary_path", "env", "flags_template"}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Fatalf("on-disk JSON missing %q field; got keys %v", key, mapKeys(raw))
		}
	}
	// No UNDOCUMENTED top-level keys may leak into the on-disk schema (drift, or
	// an accidental new serialized field that could carry secret material).
	if len(raw) != len(want) {
		t.Fatalf("on-disk JSON has %d top-level keys, want exactly %d; got %v",
			len(raw), len(want), mapKeys(raw))
	}
}

func mapKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestIsStale(t *testing.T) {
	fp := sampleFingerprint()
	cases := []struct {
		name    string
		fp      *Fingerprint
		current string
		want    bool
	}{
		{"nil fingerprint", nil, "2.1.150", true},
		{"empty current", fp, "", true},
		{"match", fp, "2.1.150", false},
		{"mismatch", fp, "2.1.200", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStale(tc.fp, tc.current); got != tc.want {
				t.Fatalf("IsStale = %v, want %v", got, tc.want)
			}
		})
	}
}
