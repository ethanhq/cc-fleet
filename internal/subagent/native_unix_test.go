//go:build !windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// nativeFake plants a fake claude that records argv + env and emits a success
// envelope, and points loadFP at it. Returns the argv and env capture paths.
func nativeFake(t *testing.T) (argvLog, envLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	envLog = filepath.Join(dir, "env.log")
	t.Setenv("CCF_ARGS_LOG", argvLog)
	t.Setenv("CCF_ENV_LOG", envLog)
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$CCF_ARGS_LOG"; done
env > "$CCF_ENV_LOG"
cat > /dev/null
printf '%s' '` + successEnvelope + `'
`
	fakeClaude := writeFakeBin(t, script)
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })
	return argvLog, envLog
}

// A native run must work with NO usable providers.toml (a malformed file must
// not gate it), write no profile, omit --settings and --model, and scrub an
// inherited ANTHROPIC_BASE_URL so the child can't be rerouted off Anthropic.
func TestRun_NativeLeaf_ArgvEnvAndBadConfig(t *testing.T) {
	argvLog, envLog := nativeFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	// Malformed providers.toml — a provider run would die at config.Load.
	cfgDir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "providers.toml"), []byte("not toml ["), 0o600); err != nil {
		t.Fatalf("write toml: %v", err)
	}
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1/evil")
	t.Setenv("ANTHROPIC_API_KEY", "user-own-env-key")

	res := Run(context.Background(), Request{Provider: "claude", Prompt: "hi", JSON: true})
	if !res.OK {
		t.Fatalf("native run failed: %+v", res)
	}
	if res.Provider != "claude" {
		t.Fatalf("Provider = %q, want claude", res.Provider)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("argv log: %v", err)
	}
	for _, banned := range []string{"--settings", "--model"} {
		if strings.Contains(string(argv), banned) {
			t.Fatalf("argv contains %s for a model-less native run:\n%s", banned, argv)
		}
	}
	envOut, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("env log: %v", err)
	}
	// Keys never ride env — the native child authenticates only from claude's
	// own stored login; routing overrides never survive either.
	for _, banned := range []string{"ANTHROPIC_BASE_URL=", "ANTHROPIC_API_KEY="} {
		if strings.Contains(string(envOut), banned) {
			t.Fatalf("child env carries %s — the scrub must be uniform for native:\n%s", banned, envOut)
		}
	}
	// No profile may exist for the pseudo-provider.
	if _, err := os.Stat(filepath.Join(home, ".claude", "profiles", "claude.json")); !os.IsNotExist(err) {
		t.Fatal("a profile was written for the native leaf")
	}
}

// Even with no routing override in sight, the lead's env credentials never
// reach the native child — keys never ride env; the child authenticates only
// from claude's own stored login (env-key-only setups use a configured
// `anthropic` provider instead).
func TestRun_NativeLeaf_CredsAlwaysScrubbed(t *testing.T) {
	_, envLog := nativeFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "user-own-env-key")

	res := Run(context.Background(), Request{Provider: "claude", Prompt: "hi", JSON: true})
	if !res.OK {
		t.Fatalf("native run failed: %+v", res)
	}
	envOut, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("env log: %v", err)
	}
	if strings.Contains(string(envOut), "user-own-env-key") {
		t.Fatal("child env carries the lead's ANTHROPIC_API_KEY — keys must never ride env, native included")
	}
}

// A pre-reservation providers.toml row named `claude` must fail the native
// lane with the migration error — never silently reroute the caller's
// configured provider onto the subscription.
func TestRun_NativeLeaf_ExistingRowFailsReserved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	argvLog, _ := nativeFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[claude]
base_url        = "https://example.invalid/anthropic"
default_model   = "claude-x"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "claude.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Request{Provider: "claude", Prompt: "hi"})
	if res.OK || res.ErrorCode != ErrCodeProviderReserved {
		t.Fatalf("got OK=%v code=%q, want %s", res.OK, res.ErrorCode, ErrCodeProviderReserved)
	}
	if _, err := os.Stat(argvLog); !os.IsNotExist(err) {
		t.Fatal("claude was exec'd despite the reserved-row guard")
	}
}

// A syntax error elsewhere in providers.toml must not disable the reserved-row
// billing guard: the raw table scan still finds the `[claude]` table.
func TestRun_NativeLeaf_MalformedConfigWithReservedRowStillGuarded(t *testing.T) {
	argvLog, _ := nativeFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := "version = 1\n\n[claude]\nbase_url = \"https://example.invalid\"\n\nnot toml [\n"
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Request{Provider: "claude", Prompt: "hi"})
	if res.OK || res.ErrorCode != ErrCodeProviderReserved {
		t.Fatalf("got OK=%v code=%q, want %s despite the malformed file", res.OK, res.ErrorCode, ErrCodeProviderReserved)
	}
	if _, err := os.Stat(argvLog); !os.IsNotExist(err) {
		t.Fatal("claude was exec'd despite the reserved-row guard")
	}
}

func TestRun_NativeLeaf_ExplicitModelPassesThrough(t *testing.T) {
	argvLog, _ := nativeFake(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	res := Run(context.Background(), Request{Provider: "claude", Model: "opus", Prompt: "hi", JSON: true})
	if !res.OK {
		t.Fatalf("native run failed: %+v", res)
	}
	argv, _ := os.ReadFile(argvLog)
	if !strings.Contains(string(argv), "--model\nopus") {
		t.Fatalf("argv missing --model opus:\n%s", argv)
	}
}

func TestRun_NativeLeaf_SlotKeywordsRejected(t *testing.T) {
	argvLog, _ := nativeFake(t)
	for _, kw := range []string{"default", "strong", "fast"} {
		res := Run(context.Background(), Request{Provider: "claude", Model: kw, Prompt: "hi"})
		if res.OK || res.ErrorCode != ErrCodeBadArgs {
			t.Fatalf("Model=%q: got OK=%v code=%q, want %s", kw, res.OK, res.ErrorCode, ErrCodeBadArgs)
		}
	}
	if _, err := os.Stat(argvLog); !os.IsNotExist(err) {
		t.Fatal("claude was exec'd despite the slot-keyword rejection")
	}
}
