//go:build !windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A provider run passes its slot's effort as --effort: a slot keyword picks that
// slot's level (blank → effort), a literal model id runs at the default slot's, and
// a provider with no effort passes none. An off slot passes --thinking disabled
// instead.
func TestRun_SlotEffortReachesArgv(t *testing.T) {
	const slotted = `
[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
strong_model    = "glm-5"
effort          = "high"
fast_effort     = "low"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	offDefault := strings.Replace(slotted, `effort          = "high"`, `effort          = "off"`, 1)
	cases := []struct {
		name, toml, model, wantModel, wantEffort, wantThinking string
	}{
		{"default slot", slotted, "", "glm-4.6", "high", ""},
		{"fast slot", slotted, "fast", "glm-4.6", "low", ""},
		{"blank strong effort follows effort", slotted, "strong", "glm-5", "high", ""},
		{"literal id runs at the default effort", slotted, "glm-x", "glm-x", "high", ""},
		{"no effort configured", "", "fast", "glm-4.6", "", ""},
		{"off default slot", offDefault, "", "glm-4.6", "", "disabled"},
		{"blank strong effort follows off", offDefault, "strong", "glm-5", "", "disabled"},
		{"fast slot keeps its level under an off default", offDefault, "fast", "glm-4.6", "low", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argvLog, _ := nativeFake(t)
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			t.Setenv("HOME", t.TempDir())
			writeMinimalProviders(t, xdg)
			if tc.toml != "" {
				path := filepath.Join(xdg, "cc-fleet", "providers.toml")
				if err := os.WriteFile(path, []byte("version = 1\n"+tc.toml), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			res := Run(context.Background(), Request{Provider: "glm", Model: tc.model, Prompt: "hi", JSON: true})
			if !res.OK {
				t.Fatalf("run failed: %+v", res)
			}
			data, err := os.ReadFile(argvLog)
			if err != nil {
				t.Fatal(err)
			}
			argv := strings.Split(strings.TrimSpace(string(data)), "\n")
			if got := flagValue(argv, "--model"); got != tc.wantModel {
				t.Errorf("--model = %q, want %q (argv %q)", got, tc.wantModel, argv)
			}
			if got := flagValue(argv, "--effort"); got != tc.wantEffort {
				t.Errorf("--effort = %q, want %q (argv %q)", got, tc.wantEffort, argv)
			}
			if got := flagValue(argv, "--thinking"); got != tc.wantThinking {
				t.Errorf("--thinking = %q, want %q (argv %q)", got, tc.wantThinking, argv)
			}
		})
	}
}

// flagValue is the token after flag in argv, or "" when flag is absent.
func flagValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}
