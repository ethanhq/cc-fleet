package subagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// successEnvelope is a minimal claude `--output-format json` result the
// classifier accepts as a success.
const successEnvelope = `{"type":"result","subtype":"success","is_error":false,` +
	`"result":"NATIVE_OK","total_cost_usd":0.01,` +
	`"usage":{"input_tokens":3,"output_tokens":2},"session_id":"s-native","num_turns":1}`

// The raw fallback recognizes spaced and quoted TOML table forms, and an
// existing-but-unreadable file fails closed instead of guessing.
func TestHasReservedRow_TableFormsAndReadFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based read denial doesn't work on windows")
	}
	for _, header := range []string{"[claude]", "[ claude ]", `["claude"]`, `['claude']`} {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "cc-fleet")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		bad := "version = 1\n\n" + header + "\nbase_url = \"x\"\n\nnot toml [\n"
		if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		found, err := hasReservedRow()
		if err != nil || !found {
			t.Fatalf("header %q: found=%v err=%v, want found", header, found, err)
		}
	}
	if os.Geteuid() != 0 {
		xdg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", xdg)
		dir := filepath.Join(xdg, "cc-fleet")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "providers.toml")
		if err := os.WriteFile(p, []byte("not toml ["), 0o000); err != nil {
			t.Fatal(err)
		}
		if _, err := hasReservedRow(); err == nil {
			t.Fatal("an existing-but-unreadable providers.toml must fail closed")
		}
	}
}

// The just-dead empty-stdout confirm re-read keys on the capture file's
// EXISTENCE, so a native background job — whose meta legitimately has no
// SettingsPath — still gets the one-shot grace before being classified as
// vanished. (The provider-shaped sibling lives in status_vanish_test.go.)
func TestStatusForConfirmDelay_NativeMetaWithoutSettingsPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 100 * time.Millisecond
	pid := deadPID(t)
	m := jobMeta{JobID: "jobn", PID: pid, PGID: pid, Status: "running", JSON: true,
		SettingsPath: "", Provider: "claude", Model: "",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "jobn.out")
	_ = os.WriteFile(outPath, nil, 0o600)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(outPath, []byte(successEnvelope), 0o600)
	}()
	st := StatusFor("jobn")
	if st.Status != "done" {
		t.Fatalf("late-envelope native bg leaf: status=%q, want done (the re-read must not key on SettingsPath)", st.Status)
	}
}
