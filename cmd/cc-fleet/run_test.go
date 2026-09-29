package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/teammate"
)

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		dash         int
		wantProvider string
		wantExtra    []string
		wantErr      bool
	}{
		{"provider only", []string{"deepseek"}, -1, "deepseek", nil, false},
		{"no args (default)", nil, -1, "", nil, false},
		{"two positionals, no dash", []string{"a", "b"}, -1, "", nil, true},
		{"provider + passthrough", []string{"deepseek", "--resume", "x"}, 1, "deepseek", []string{"--resume", "x"}, false},
		{"default + passthrough", []string{"--resume"}, 0, "", []string{"--resume"}, false},
		{"provider + empty passthrough", []string{"deepseek"}, 1, "deepseek", nil, false},
		{"two positionals before dash", []string{"a", "b", "x"}, 2, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, extra, err := splitRunArgs(tc.args, tc.dash)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got provider=%q extra=%v", provider, extra)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
			if len(extra) != len(tc.wantExtra) {
				t.Fatalf("extra = %v, want %v", extra, tc.wantExtra)
			}
			for i := range extra {
				if extra[i] != tc.wantExtra[i] {
					t.Fatalf("extra = %v, want %v", extra, tc.wantExtra)
				}
			}
		})
	}
}

// TestResolvePermissionOverride covers the manual override flag resolution that
// runs before any run side effect.
func TestResolvePermissionOverride(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		danger  bool
		want    string
		wantErr bool
	}{
		{"no flags → infer", "", false, "", false},
		{"danger → bypass", "", true, permmode.BypassPermissions, false},
		{"explicit acceptEdits", "acceptEdits", false, "acceptEdits", false},
		{"explicit auto", "auto", false, "auto", false},
		{"explicit plan", "plan", false, "plan", false},
		{"explicit default", "default", false, "default", false},
		{"explicit bypass", "bypassPermissions", false, "bypassPermissions", false},
		// both flags → error even though they'd agree.
		{"conflict bypass+danger", "bypassPermissions", true, "", true},
		{"conflict acceptEdits+danger", "acceptEdits", true, "", true},
		// unknown mode → error.
		{"invalid mode", "garbage", false, "", true},
		{"invalid mode bypass-typo", "bypass", false, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolvePermissionOverride(tt.mode, tt.danger)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolvePermissionOverride(%q, %v) = (%q, nil), want error", tt.mode, tt.danger, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolvePermissionOverride(%q, %v) unexpected error: %v", tt.mode, tt.danger, err)
			}
			if got != tt.want {
				t.Fatalf("resolvePermissionOverride(%q, %v) = %q, want %q", tt.mode, tt.danger, got, tt.want)
			}
		})
	}
}

// removedStubArgsEnv carries the child argv (joined by \x1f) for
// TestRemovedCommandsAreStubs, which re-runs this test binary as cc-fleet.
const removedStubArgsEnv = "CCF_TEST_REMOVED_STUB_ARGS"

// TestRemovedCommandsAreStubs runs the real entry point (main) in a child
// process: whatever arguments spawn / refresh-fingerprint get, they print
// exactly one COMMAND_REMOVED envelope on stdout, nothing on stderr, and exit 1.
func TestRemovedCommandsAreStubs(t *testing.T) {
	if raw, ok := os.LookupEnv(removedStubArgsEnv); ok {
		os.Args = append([]string{"cc-fleet"}, strings.Split(raw, "\x1f")...)
		main()
		os.Exit(99) // main returned: the stub did not exit by itself
	}

	root := newRootCmd()
	for _, name := range []string{"spawn", "refresh-fingerprint"} {
		c, _, err := root.Find([]string{name})
		if err != nil || c.Name() != name {
			t.Fatalf("root.Find(%q) = %v, %v", name, c, err)
		}
		if !c.Hidden || !c.DisableFlagParsing || !c.SilenceUsage {
			t.Errorf("%s: Hidden=%v DisableFlagParsing=%v SilenceUsage=%v, want all true",
				name, c.Hidden, c.DisableFlagParsing, c.SilenceUsage)
		}
	}

	wantKeys := []string{"error_code", "error_msg", "ok", "suggestion"}
	for _, args := range [][]string{
		{"spawn"},
		{"spawn", "openrouter", "--as", "worker-1", "--team", "t", "--json"},
		{"spawn", "--help"},
		{"spawn", "--no-such-flag=1", "extra", "positional"},
		{"--verbose", "spawn", "p"},
		{"refresh-fingerprint"},
		{"refresh-fingerprint", "--probe-team", "_ccf-probe-x", "--json"},
		{"refresh-fingerprint", "-h"},
	} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRemovedCommandsAreStubs$")
		cmd.Env = append(os.Environ(), removedStubArgsEnv+"="+strings.Join(args, "\x1f"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()

		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Errorf("cc-fleet %v: exit = %v, want exit status 1 (stdout %q, stderr %q)", args, err, stdout.String(), stderr.String())
			continue
		}
		if stderr.Len() != 0 {
			t.Errorf("cc-fleet %v: stderr = %q, want nothing", args, stderr.String())
		}
		out := strings.TrimSuffix(stdout.String(), "\n")
		if strings.Contains(out, "\n") {
			t.Errorf("cc-fleet %v: stdout has more than one line: %q", args, stdout.String())
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Errorf("cc-fleet %v: stdout is not one JSON object: %v (%q)", args, err, out)
			continue
		}
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Errorf("cc-fleet %v: envelope keys = %v, want %v", args, keys, wantKeys)
		}
		if env["ok"] != false || env["error_code"] != teammate.CodeCommandRemoved {
			t.Errorf("cc-fleet %v: envelope = %v, want ok=false error_code=%s", args, env, teammate.CodeCommandRemoved)
		}
		for _, k := range []string{"error_msg", "suggestion"} {
			if s, _ := env[k].(string); s == "" {
				t.Errorf("cc-fleet %v: %s is empty", args, k)
			}
		}
	}
}
