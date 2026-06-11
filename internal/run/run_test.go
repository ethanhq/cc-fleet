package run

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// launch records what execClaude was handed, so a test can assert the argv/env
// without replacing the test process.
type launch struct {
	called    bool
	bin       string
	argv, env []string
}

// stubSeams swaps resolveBinary + execClaude for the test and returns the
// capture. binErr != nil makes binary resolution fail (no exec should happen).
func stubSeams(t *testing.T, binPath string, binErr error) *launch {
	t.Helper()
	got := &launch{}
	origResolve, origExec := resolveBinary, execClaude
	resolveBinary = func() (string, error) { return binPath, binErr }
	execClaude = func(_ *config.Provider, bin string, argv, env []string) error {
		got.called, got.bin, got.argv, got.env = true, bin, argv, env
		return nil
	}
	t.Cleanup(func() { resolveBinary, execClaude = origResolve, origExec })
	return got
}

// seedProvider writes one provider into a fresh temp-HOME providers.toml.
func seedProvider(t *testing.T, name string, enabled bool, defaultModel string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Providers[name] = &config.Provider{
		Name:           name,
		BaseURL:        "https://api." + name + ".example/anthropic",
		ModelsEndpoint: "https://api." + name + ".example/v1/models",
		DefaultModel:   defaultModel,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        enabled,
		AddedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func TestRun_ExecsWithProfileAndModel(t *testing.T) {
	seedProvider(t, "deepseek", true, "deepseek-chat")
	t.Setenv("ANTHROPIC_API_KEY", "sk-must-not-leak")
	t.Setenv("CLAUDECODE", "1")
	got := stubSeams(t, "/fake/claude", nil)

	if err := Run(Request{Provider: "deepseek"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !got.called {
		t.Fatal("execClaude was not called")
	}
	if got.bin != "/fake/claude" || len(got.argv) < 1 || got.argv[0] != "/fake/claude" {
		t.Fatalf("argv[0] must equal bin; bin=%q argv=%v", got.bin, got.argv)
	}
	want := []string{"/fake/claude", "--settings", "", "--model", "deepseek-chat"}
	if len(got.argv) != len(want) {
		t.Fatalf("argv = %v, want shape %v", got.argv, want)
	}
	if got.argv[1] != "--settings" || !strings.HasSuffix(got.argv[2], "deepseek.json") {
		t.Fatalf("argv --settings <profile> wrong: %v", got.argv)
	}
	if got.argv[3] != "--model" || got.argv[4] != "deepseek-chat" {
		t.Fatalf("argv --model wrong: %v", got.argv)
	}
	// Key-safety: the lead's creds + nested-CC markers must not reach the child.
	joined := strings.Join(got.env, "\n")
	for _, leak := range []string{"ANTHROPIC_API_KEY=", "sk-must-not-leak", "CLAUDECODE="} {
		if strings.Contains(joined, leak) {
			t.Fatalf("exec env leaked %q", leak)
		}
	}
}

func TestRun_ModelOverrideAndPassthrough(t *testing.T) {
	seedProvider(t, "deepseek", true, "deepseek-chat")
	got := stubSeams(t, "/fake/claude", nil)

	if err := Run(Request{Provider: "deepseek", Model: "deepseek-reasoner", ExtraArgs: []string{"--resume", "abc"}}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// managed flags first (--settings/--model), then passthrough last.
	want := []string{"/fake/claude", "--settings", got.argv[2], "--model", "deepseek-reasoner", "--resume", "abc"}
	if strings.Join(got.argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", got.argv, want)
	}
}

func TestRun_RejectsReservedPassthrough(t *testing.T) {
	seedProvider(t, "deepseek", true, "deepseek-chat")
	for _, bad := range [][]string{
		{"--model", "x"}, {"--settings=/tmp/x"},
		{"--permission-mode", "plan"}, {"--permission-mode=plan"},
		{"--dangerously-skip-permissions"}, {"--dangerously-skip-permissions=true"},
	} {
		got := stubSeams(t, "/fake/claude", nil)
		err := Run(Request{Provider: "deepseek", ExtraArgs: bad})
		if err == nil || !strings.Contains(err.Error(), "managed by cc-fleet") {
			t.Fatalf("passthrough %v: err = %v, want a managed-by-cc-fleet rejection", bad, err)
		}
		if got.called {
			t.Fatalf("passthrough %v reached exec", bad)
		}
	}
}

func TestRun_PermissionModeReachesArgv(t *testing.T) {
	seedProvider(t, "deepseek", true, "deepseek-chat")
	for mode, wantTail := range map[string]string{
		"bypassPermissions": "--dangerously-skip-permissions",
		"acceptEdits":       "--permission-mode acceptEdits",
		"auto":              "--permission-mode auto",
		"plan":              "--permission-mode plan", // forwarded faithfully (not collapsed)
	} {
		got := stubSeams(t, "/fake/claude", nil)
		if err := Run(Request{Provider: "deepseek", PermissionMode: mode}); err != nil {
			t.Fatalf("Run(%q): %v", mode, err)
		}
		if !strings.HasSuffix(strings.Join(got.argv, " "), wantTail) {
			t.Fatalf("PermissionMode %q: argv = %v, want suffix %q", mode, got.argv, wantTail)
		}
	}
}

func TestRun_GatesFailBeforeExec(t *testing.T) {
	cases := []struct {
		name           string
		setup          func(t *testing.T)
		req            Request
		binErr         error
		wantErrSubstr  string
		wantStaleErrIs bool
	}{
		{"invalid provider name", func(t *testing.T) { seedProvider(t, "deepseek", true, "deepseek-chat") },
			Request{Provider: "%bad"}, nil, "provider", false},
		{"unknown provider", func(t *testing.T) { seedProvider(t, "deepseek", true, "deepseek-chat") },
			Request{Provider: "ghost"}, nil, "not configured", false},
		{"disabled provider", func(t *testing.T) { seedProvider(t, "deepseek", false, "deepseek-chat") },
			Request{Provider: "deepseek"}, nil, "disabled", false},
		{"no claude binary", func(t *testing.T) { seedProvider(t, "deepseek", true, "deepseek-chat") },
			Request{Provider: "deepseek"}, fingerprint.ErrFingerprintStale, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			got := stubSeams(t, "/fake/claude", tc.binErr)
			err := Run(tc.req)
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if tc.wantStaleErrIs && !errors.Is(err, fingerprint.ErrFingerprintStale) {
				t.Fatalf("err = %v, want ErrFingerprintStale", err)
			}
			if tc.wantErrSubstr != "" && !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErrSubstr)
			}
			if got.called {
				t.Fatal("a rejected request reached exec")
			}
		})
	}
}

func TestBuildArgv(t *testing.T) {
	base := buildArgv("/b", "/p.json", "m", nil, nil)
	if strings.Join(base, " ") != "/b --settings /p.json --model m" {
		t.Fatalf("buildArgv base = %v", base)
	}
	// managed flags FIRST (--settings/--model + perm), then passthrough LAST.
	full := buildArgv("/b", "/p.json", "m", []string{"--dangerously-skip-permissions"}, []string{"--resume", "x"})
	if strings.Join(full, " ") != "/b --settings /p.json --model m --dangerously-skip-permissions --resume x" {
		t.Fatalf("buildArgv full = %v", full)
	}
	// A "--" in the passthrough must not precede the managed flags, or claude
	// would read --settings/--model as positionals after the terminator.
	dashes := buildArgv("/b", "/p.json", "m", nil, []string{"--", "-p", "hi"})
	if strings.Join(dashes, " ") != "/b --settings /p.json --model m -- -p hi" {
		t.Fatalf("buildArgv with -- = %v", dashes)
	}
}

func TestReservedFlag(t *testing.T) {
	for in, want := range map[string]string{
		"--model": "--model", "--model=x": "--model",
		"--settings": "--settings", "--settings=/p": "--settings",
		"--resume": "", "--modelx": "", "foo": "",
	} {
		if got := reservedFlag(in); got != want {
			t.Errorf("reservedFlag(%q) = %q, want %q", in, got, want)
		}
	}
}
