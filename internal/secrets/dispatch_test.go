package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// setupConfig redirects XDG_CONFIG_HOME at a fresh temp dir and writes
// vendors.toml + an optional secret file. It returns the cc-fleet config root
// so callers can poke at paths if they need to.
//
// secretContents is written verbatim under <ConfigDir>/secrets/<secretRef> iff
// secretRef != "" and we pass writeSecret=true.
func setupConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// Ensure HOME doesn't bleed into ConfigDir resolution.
	t.Setenv("HOME", filepath.Join(xdg, "fakehome"))

	if cfg != nil {
		path, err := config.VendorsPath()
		if err != nil {
			t.Fatalf("VendorsPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir config dir: %v", err)
		}
		if err := config.SaveToPath(cfg, path); err != nil {
			t.Fatalf("SaveToPath: %v", err)
		}
	}

	root, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	return root
}

// writeSecretFile drops contents into <ConfigDir>/secrets/<ref> with 0600 perms.
func writeSecretFile(t *testing.T, ref string, contents []byte) {
	t.Helper()
	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir secrets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ref), contents, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
}

// fileVendor returns a single-vendor Config that uses the file backend.
func fileVendor(name, ref string) *config.Config {
	return &config.Config{
		Version: config.SchemaVersion,
		Vendors: map[string]*config.Vendor{
			name: {
				Name:           name,
				BaseURL:        "https://api." + name + ".com/anthropic",
				DefaultModel:   name + "-latest",
				ModelsEndpoint: "https://api." + name + ".com/v1/models",
				SecretBackend:  "file",
				SecretRef:      ref,
				Enabled:        true,
				AddedAt:        time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
			},
		},
	}
}

// An openai-* provider carries a real key via a normal backend (b1): keyget
// returns that key for the daemon to forward, never the codex handshake secret.
func TestKeyget_OpenAIChat_ReturnsRealKey(t *testing.T) {
	cfg := &config.Config{
		Version: config.SchemaVersion,
		Vendors: map[string]*config.Vendor{
			"groq": {
				Name:           "groq",
				BaseURL:        "http://127.0.0.1:17240/",
				DefaultModel:   "llama-3.3",
				ModelsEndpoint: "https://api.groq.com/openai/v1/models",
				SecretBackend:  "file",
				SecretRef:      "groq.key",
				Protocol:       config.ProtocolOpenAIChat,
				UpstreamURL:    "https://api.groq.com/openai/v1",
				Enabled:        true,
				AddedAt:        time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	setupConfig(t, cfg)
	writeSecretFile(t, "groq.key", []byte("sk-real-groq-key"))

	got, err := Keyget("groq")
	if err != nil {
		t.Fatalf("Keyget: %v", err)
	}
	if string(got) != "sk-real-groq-key" {
		t.Fatalf("Keyget = %q, want the real key (b1, not the handshake secret)", got)
	}
}

func TestKeyget_File_OK(t *testing.T) {
	setupConfig(t, fileVendor("deepseek", "deepseek.key"))
	writeSecretFile(t, "deepseek.key", []byte("sk-deepseek-abc123"))

	got, err := Keyget("deepseek")
	if err != nil {
		t.Fatalf("Keyget: %v", err)
	}
	if want := "sk-deepseek-abc123"; string(got) != want {
		t.Fatalf("Keyget = %q, want %q", got, want)
	}
}

func TestKeyget_File_TrimsNewline(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"trailing_LF", "sk-key\n", "sk-key"},
		{"trailing_CRLF", "sk-key\r\n", "sk-key"},
		{"multiple_trailing", "sk-key\n\n\n", "sk-key"},
		{"no_trailing", "sk-key", "sk-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupConfig(t, fileVendor("deepseek", "deepseek.key"))
			writeSecretFile(t, "deepseek.key", []byte(tc.raw))

			got, err := Keyget("deepseek")
			if err != nil {
				t.Fatalf("Keyget: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Keyget = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeyget_File_MissingFile(t *testing.T) {
	// Config points at a secret file we never create, and there is no keys.json
	// either, so this is an empty key set: keyget reports ErrNoEnabledKey (and
	// writes no key bytes) rather than a read error.
	setupConfig(t, fileVendor("deepseek", "deepseek.key"))

	got, err := Keyget("deepseek")
	if err == nil {
		t.Fatalf("Keyget: want error for missing secret, got nil")
	}
	if !errors.Is(err, ErrNoEnabledKey) {
		t.Fatalf("Keyget err = %v, want wrapped ErrNoEnabledKey", err)
	}
	if len(got) != 0 {
		t.Fatalf("Keyget returned %d key bytes on error, want none", len(got))
	}
}

func TestKeyget_UnknownVendor(t *testing.T) {
	// Valid config, but ask for a vendor that isn't in it.
	setupConfig(t, fileVendor("deepseek", "deepseek.key"))

	_, err := Keyget("nope")
	if err == nil {
		t.Fatalf("Keyget: want error for unknown vendor, got nil")
	}
	if !strings.Contains(err.Error(), "unknown vendor") {
		t.Fatalf("error %q should mention unknown vendor", err.Error())
	}
}

func TestKeyget_UnknownBackend(t *testing.T) {
	// LoadFromPath is default-strict, so a vendors.toml with an unknown
	// secret_backend is rejected at Load, before the dispatch switch sees it;
	// the user-visible failure is the validator's "secret_backend %q invalid".
	//
	// We hand-write the vendors.toml because SaveToPath would refuse to persist
	// it (Validate rejects it). The test still asserts the bad backend value is
	// named and no key bytes leak — the security property the consumer needs.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", filepath.Join(xdg, "fakehome"))

	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `version = 1

[weirdo]
base_url = "https://x.example.com/anthropic"
default_model = "x-latest"
models_endpoint = "https://x.example.com/v1/models"
secret_backend = "weird"
secret_ref = "x.key"
enabled = true
added_at = 2026-05-24T00:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "vendors.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write vendors.toml: %v", err)
	}

	got, err := Keyget("weirdo")
	if err == nil {
		t.Fatalf("Keyget: want error for unknown backend, got nil")
	}
	msg := err.Error()
	// The error must call out the bad value (so the user can fix it) and
	// must name the field. Pre-P2 this came from dispatch.go; post-P2 the
	// same information comes from the validator wrapped via LoadFromPath.
	if !strings.Contains(msg, "weird") {
		t.Fatalf("error %q should call out the bad backend value", msg)
	}
	if !strings.Contains(msg, "secret_backend") {
		t.Fatalf("error %q should name the field that failed", msg)
	}
	if len(got) != 0 {
		t.Fatalf("Keyget returned %d key bytes on error, want none", len(got))
	}
}

// ---------------------------------------------------------------------------
// 1password / vault / keyring backends
// ---------------------------------------------------------------------------

// backendVendor returns a single-vendor Config that uses an arbitrary secret
// backend + ref. config.Validate accepts all five backend names and only
// presence-checks secret_ref, so even a deliberately malformed ref persists
// (it fails at keyget parse time, which is what these tests exercise).
func backendVendor(name, backend, ref string) *config.Config {
	return &config.Config{
		Version: config.SchemaVersion,
		Vendors: map[string]*config.Vendor{
			name: {
				Name:           name,
				BaseURL:        "https://api." + name + ".com/anthropic",
				DefaultModel:   name + "-latest",
				ModelsEndpoint: "https://api." + name + ".com/v1/models",
				SecretBackend:  backend,
				SecretRef:      ref,
				Enabled:        true,
				AddedAt:        time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC),
			},
		},
	}
}

// fakeCLI installs an executable named `name` on PATH that records its argv
// (one token per line) to $FAKE_ARGS, prints $FAKE_OUT to stdout, and exits
// $FAKE_EXIT (default 0). Same fake-CLI-on-PATH scheme as
// internal/panevis/panevis_test.go. Returns the args-log path so a test can
// assert the exact argv the backend invoked.
func fakeCLI(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("keyget CLI-backend tests use a #!/bin/sh fake binary not runnable on windows")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_ARGS"; done
printf '%s' "$FAKE_OUT"
exit "${FAKE_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGS", argsPath)
	return argsPath
}

// readArgs returns the argv the fake CLI recorded (nil if it was never run).
func readArgs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// wantArgs asserts the recorded argv equals want exactly.
func wantArgs(t *testing.T, path string, want ...string) {
	t.Helper()
	got := readArgs(t, path)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("backend argv = %v, want %v", got, want)
	}
}

func TestKeyget_Pass_OK(t *testing.T) {
	setupConfig(t, backendVendor("ps", "pass", "cc-fleet/glm"))
	args := fakeCLI(t, "pass")
	t.Setenv("FAKE_OUT", "sk-pass-FAKE-000\n")

	got, err := Keyget("ps")
	if err != nil {
		t.Fatalf("Keyget pass: %v", err)
	}
	if string(got) != "sk-pass-FAKE-000" {
		t.Fatalf("Keyget = %q, want trimmed fake key", got)
	}
	wantArgs(t, args, "show", "cc-fleet/glm")
}

func TestKeyget_1Password_OK(t *testing.T) {
	setupConfig(t, backendVendor("op1", "1password", "op://Personal/glm/credential"))
	args := fakeCLI(t, "op")
	t.Setenv("FAKE_OUT", "sk-op-FAKE-123\n")

	got, err := Keyget("op1")
	if err != nil {
		t.Fatalf("Keyget 1password: %v", err)
	}
	if string(got) != "sk-op-FAKE-123" {
		t.Fatalf("Keyget = %q, want trimmed fake key", got)
	}
	wantArgs(t, args, "read", "op://Personal/glm/credential")
}

func TestKeyget_Vault_OK(t *testing.T) {
	setupConfig(t, backendVendor("vlt", "vault", "secret/data/cc-fleet/glm#api_key"))
	args := fakeCLI(t, "vault")
	t.Setenv("FAKE_OUT", "sk-vault-FAKE-456\r\n") // CRLF must be trimmed

	got, err := Keyget("vlt")
	if err != nil {
		t.Fatalf("Keyget vault: %v", err)
	}
	if string(got) != "sk-vault-FAKE-456" {
		t.Fatalf("Keyget = %q, want trimmed fake key", got)
	}
	wantArgs(t, args, "kv", "get", "-field=api_key", "secret/data/cc-fleet/glm")
}

// TestKeyget_Vault_SplitsOnLastHash pins the "split on the LAST '#'" contract so
// a '#' inside the KV path is preserved in the path, not the field.
func TestKeyget_Vault_SplitsOnLastHash(t *testing.T) {
	setupConfig(t, backendVendor("vlt", "vault", "secret/data/a#b#api_key"))
	args := fakeCLI(t, "vault")
	t.Setenv("FAKE_OUT", "k")

	if _, err := Keyget("vlt"); err != nil {
		t.Fatalf("Keyget vault: %v", err)
	}
	wantArgs(t, args, "kv", "get", "-field=api_key", "secret/data/a#b")
}

func TestKeyget_Keyring_OK(t *testing.T) {
	setupConfig(t, backendVendor("kr", "keyring", "service cc-fleet account glm"))
	args := fakeCLI(t, "secret-tool")
	t.Setenv("FAKE_OUT", "sk-keyring-FAKE-789\n")

	got, err := Keyget("kr")
	if err != nil {
		t.Fatalf("Keyget keyring: %v", err)
	}
	if string(got) != "sk-keyring-FAKE-789" {
		t.Fatalf("Keyget = %q, want trimmed fake key", got)
	}
	wantArgs(t, args, "lookup", "service", "cc-fleet", "account", "glm")
}

// TestKeyget_Backend_CLIMissing checks every CLI backend returns a clean,
// backend-named error (not a panic, no key bytes) when its tool is absent.
func TestKeyget_Backend_CLIMissing(t *testing.T) {
	cases := []struct{ backend, ref string }{
		{"pass", "cc-fleet/glm"},
		{"1password", "op://Personal/glm/credential"},
		{"vault", "secret/data/glm#api_key"},
		{"keyring", "service cc-fleet account glm"},
	}
	for _, c := range cases {
		t.Run(c.backend, func(t *testing.T) {
			setupConfig(t, backendVendor("v", c.backend, c.ref))
			// Point PATH at an empty dir so the backend CLI cannot be found.
			t.Setenv("PATH", t.TempDir())

			got, err := Keyget("v")
			if err == nil {
				t.Fatalf("Keyget %s with missing CLI: want error, got nil", c.backend)
			}
			if len(got) != 0 {
				t.Fatalf("Keyget %s returned %d bytes on missing CLI, want 0", c.backend, len(got))
			}
			if !strings.Contains(err.Error(), c.backend) {
				t.Fatalf("error %q should name the %q backend", err.Error(), c.backend)
			}
		})
	}
}

// TestKeyget_Vault_MalformedRefNoExec proves a malformed vault ref fails during
// parsing — before any exec — so a key can never be fetched or leaked.
func TestKeyget_Vault_MalformedRefNoExec(t *testing.T) {
	const sentinel = "sk-VAULT-SENTINEL-must-never-appear"
	setupConfig(t, backendVendor("v", "vault", "no-hash-here"))
	args := fakeCLI(t, "vault")
	t.Setenv("FAKE_OUT", sentinel)

	got, err := Keyget("v")
	if err == nil {
		t.Fatalf("Keyget vault malformed ref: want error, got nil")
	}
	if len(got) != 0 || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("malformed vault ref leaked/returned key: bytes=%d err=%q", len(got), err.Error())
	}
	if a := readArgs(t, args); len(a) != 0 {
		t.Fatalf("vault CLI was invoked on a malformed ref (argv=%v); parse must fail first", a)
	}
	if !strings.Contains(err.Error(), "vault") {
		t.Fatalf("error %q should name the vault backend", err.Error())
	}
}

// TestKeyget_Keyring_OddRefNoExec is the keyring analogue: an odd token count
// must fail at parse time, never invoking secret-tool.
func TestKeyget_Keyring_OddRefNoExec(t *testing.T) {
	const sentinel = "sk-KEYRING-SENTINEL-must-never-appear"
	setupConfig(t, backendVendor("k", "keyring", "service cc-fleet account")) // 3 tokens (odd)
	args := fakeCLI(t, "secret-tool")
	t.Setenv("FAKE_OUT", sentinel)

	got, err := Keyget("k")
	if err == nil {
		t.Fatalf("Keyget keyring odd ref: want error, got nil")
	}
	if len(got) != 0 || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("odd keyring ref leaked/returned key: bytes=%d err=%q", len(got), err.Error())
	}
	if a := readArgs(t, args); len(a) != 0 {
		t.Fatalf("secret-tool was invoked on an odd ref (argv=%v); parse must fail first", a)
	}
	if !strings.Contains(err.Error(), "keyring") {
		t.Fatalf("error %q should name the keyring backend", err.Error())
	}
}

func TestParseVaultRef(t *testing.T) {
	ok := []struct{ ref, path, field string }{
		{"secret/data/glm#api_key", "secret/data/glm", "api_key"},
		{"a#b#c", "a#b", "c"}, // split on LAST '#'
	}
	for _, c := range ok {
		p, f, err := parseVaultRef(c.ref)
		if err != nil || p != c.path || f != c.field {
			t.Errorf("parseVaultRef(%q) = (%q,%q,%v), want (%q,%q,nil)", c.ref, p, f, err, c.path, c.field)
		}
	}
	for _, bad := range []string{"", "no-hash", "#field", "path#", "#"} {
		if _, _, err := parseVaultRef(bad); err == nil {
			t.Errorf("parseVaultRef(%q): want error, got nil", bad)
		}
	}
}

func TestParseKeyringRef(t *testing.T) {
	got, err := parseKeyringRef("service cc-fleet account glm")
	want := []string{"service", "cc-fleet", "account", "glm"}
	if err != nil || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("parseKeyringRef(even) = (%v,%v), want (%v,nil)", got, err, want)
	}
	for _, bad := range []string{"", "   ", "one", "a b c"} {
		if _, err := parseKeyringRef(bad); err == nil {
			t.Errorf("parseKeyringRef(%q): want error for empty/odd, got nil", bad)
		}
	}
}

// TestSelectKey_RejectsInvalidRotation: defense-in-depth. A
// well-formed vendors.toml has been Validated at load, but a direct caller
// (test fixture, hand-edited keys.json bypass, etc.) could still hand selectKey
// a bogus rotation string. The typed dispatch must refuse explicitly rather
// than fall through to a silent default-off / default-round_robin.
func TestSelectKey_RejectsInvalidRotation(t *testing.T) {
	enabled := []KeyEntry{
		{Label: "a", Key: "sk-a", Enabled: true},
		{Label: "b", Key: "sk-b", Enabled: true},
	}
	_, err := selectKey("glm", "typo-strategy", enabled)
	if err == nil {
		t.Fatal("selectKey: want error for invalid rotation, got nil")
	}
	if !strings.Contains(err.Error(), "invalid key_rotation") {
		t.Fatalf("selectKey error = %q, want it to mention invalid key_rotation", err)
	}
}

// TestSelectKey_TypedDispatchOff: "" / "off" must always select the first
// enabled key (deterministic). Single-key case is short-circuited so this
// is the multi-enabled-key path.
func TestSelectKey_TypedDispatchOff(t *testing.T) {
	enabled := []KeyEntry{
		{Label: "a", Key: "sk-aaa", Enabled: true},
		{Label: "b", Key: "sk-bbb", Enabled: true},
	}
	for _, r := range []string{"", "off"} {
		got, err := selectKey("glm", r, enabled)
		if err != nil {
			t.Fatalf("selectKey(%q): %v", r, err)
		}
		if string(got) != "sk-aaa" {
			t.Fatalf("selectKey(%q) = %q, want sk-aaa (first enabled)", r, got)
		}
	}
}
