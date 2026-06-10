package profile

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// helperFromProfile renders a profile for sampleProvider() with helperBinary and
// returns the apiKeyHelper string from the generated JSON.
func helperFromProfile(t *testing.T, helperBinary string) string {
	t.Helper()
	data, err := GenerateForProvider(sampleProvider(), helperBinary)
	if err != nil {
		t.Fatalf("GenerateForProvider: %v", err)
	}
	var back struct {
		APIKeyHelper string `json:"apiKeyHelper"`
	}
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return back.APIKeyHelper
}

// TestPosixQuote_SafePathVerbatim documents the byte-stable common case: a
// space/metachar-free install path and the (already grammar-validated) provider
// name are emitted WITHOUT quotes, so existing profiles stay unchanged.
func TestPosixQuote_SafePathVerbatim(t *testing.T) {
	for _, s := range []string{"/usr/local/bin/cc-fleet", "deepseek", "glm-4", "/a/b_c/d.bin"} {
		if got := posixQuote(s); got != s {
			t.Fatalf("posixQuote(%q) = %q, want verbatim", s, got)
		}
	}
}

// TestPosixQuote_WrapsUnsafe verifies a path with a space / shell metacharacter
// is single-quoted, with embedded single quotes escaped as '\”.
func TestPosixQuote_WrapsUnsafe(t *testing.T) {
	cases := map[string]string{
		"/Users/Jane Doe/bin/cc-fleet": `'/Users/Jane Doe/bin/cc-fleet'`,
		"/opt/$(whoami)/cc-fleet":      `'/opt/$(whoami)/cc-fleet'`,
		"a'b":                          `'a'\''b'`,
		"":                             `''`,
	}
	for in, want := range cases {
		if got := posixQuote(in); got != want {
			t.Fatalf("posixQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestApiKeyHelper_ExecutesThroughShell_WithSpacePath: a profile generated for a
// helper path containing a SPACE must execute correctly when the apiKeyHelper
// string is run through `/bin/sh -c` against a fake helper. The fake helper
// prints a sentinel "key" so we confirm the shell located + ran the right binary
// (the real key never enters the string — keyget resolves it at runtime).
func TestApiKeyHelper_ExecutesThroughShell_WithSpacePath(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Jane Doe", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	helper := filepath.Join(dir, "cc-fleet")
	// Fake helper: echoes a sentinel only when called as `<bin> keyget deepseek`
	// (deepseek == sampleProvider().Name).
	script := "#!/bin/sh\nif [ \"$1\" = keyget ] && [ \"$2\" = deepseek ]; then echo SENTINEL_KEY_OK; else echo WRONG_ARGS; fi\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cmdStr := helperFromProfile(t, helper)
	out, err := exec.Command("/bin/sh", "-c", cmdStr).Output()
	if err != nil {
		t.Fatalf("run apiKeyHelper %q: %v", cmdStr, err)
	}
	if strings.TrimSpace(string(out)) != "SENTINEL_KEY_OK" {
		t.Fatalf("apiKeyHelper %q produced %q, want SENTINEL_KEY_OK", cmdStr, out)
	}
}

// TestApiKeyHelper_NoInjection_WithMetacharPath proves the quoting defeats shell
// command substitution: a helper path containing `$(...)` must NOT execute the
// substitution. We place the helper under a directory whose name carries a
// `$(touch PWNED)` payload and assert no PWNED file is created and the right
// helper still runs.
func TestApiKeyHelper_NoInjection_WithMetacharPath(t *testing.T) {
	base := t.TempDir()
	pwned := filepath.Join(base, "PWNED")
	// Directory name embeds a command-substitution payload as LITERAL bytes.
	dir := filepath.Join(base, "a $(touch "+pwned+")")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	helper := filepath.Join(dir, "cc-fleet")
	script := "#!/bin/sh\necho SENTINEL_KEY_OK\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	cmdStr := helperFromProfile(t, helper)
	out, _ := exec.Command("/bin/sh", "-c", cmdStr).Output()
	if _, err := os.Stat(pwned); err == nil {
		t.Fatalf("command substitution executed — PWNED file created; apiKeyHelper not safely quoted: %q", cmdStr)
	}
	if strings.TrimSpace(string(out)) != "SENTINEL_KEY_OK" {
		t.Fatalf("apiKeyHelper %q produced %q, want SENTINEL_KEY_OK", cmdStr, out)
	}
}
