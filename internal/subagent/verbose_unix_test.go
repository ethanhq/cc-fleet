//go:build !windows

package subagent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// A verbose sync run traces its steps WITHOUT leaking the prompt, argv values,
// or any secret — including a key that matches none of the redact patterns
// (the allowlist, not the regex, is the guarantee).
func TestRun_VerboseTraceAllowlist(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	// A non-pattern canary secret on disk where the provider's secret_ref points.
	const canaryKey = "TOPSECRET-nonpattern-9f7e2b"
	secretsDir := filepath.Join(xdg, "cc-fleet", "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "glm.key"), []byte(canaryKey), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	orig := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = orig })

	const promptCanary = "prompt-text-canary-31c8"
	var buf bytes.Buffer
	res := Run(context.Background(), Request{
		Provider: "glm", Prompt: promptCanary, JSON: true, Diag: diag.New(&buf),
	})
	if !res.OK {
		t.Fatalf("sync run failed: %+v", res)
	}

	out := buf.String()
	for _, marker := range []string{
		"subagent: fingerprint gate ok",
		"subagent: profile written",
		"argv",
		"subagent: claude exited code 0",
	} {
		if !strings.Contains(out, marker) {
			t.Fatalf("verbose trace missing %q:\n%s", marker, out)
		}
	}
	for _, leak := range []string{canaryKey, promptCanary} {
		if strings.Contains(out, leak) {
			t.Fatalf("verbose trace leaked %q:\n%s", leak, out)
		}
	}
}

// A verbose run with a nil logger is the default path: same Result, no trace.
func TestRun_NilDiagIsNoOp(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	orig := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = orig })

	if res := Run(context.Background(), Request{Provider: "glm", Prompt: "hi", JSON: true}); !res.OK {
		t.Fatalf("nil-diag run failed: %+v", res)
	}
}

// A verbose background launch traces only launch metadata (job id, pid,
// capture path, detach) — the parent releases the child and observes no more.
func TestLaunchBackground_VerboseLaunchMetadata(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	orig := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = orig })

	var buf bytes.Buffer
	res := Run(context.Background(), Request{
		Provider: "glm", Prompt: "hi", JSON: true, Background: true, Diag: diag.New(&buf),
	})
	if !res.OK || res.JobID == "" {
		t.Fatalf("background launch failed: %+v", res)
	}
	out := buf.String()
	if !strings.Contains(out, "background job "+res.JobID+" started") {
		t.Fatalf("missing launch-metadata line:\n%s", out)
	}
	if !strings.Contains(out, "background job "+res.JobID+" detached") {
		t.Fatalf("missing detach line:\n%s", out)
	}
}
