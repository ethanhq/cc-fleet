//go:build !windows

package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunClaude_OutputCapKillsGroup: a child that floods stdout past the cap is
// SIGKILLed (the whole group), runClaude returns errOutputTooLarge promptly (not
// at the generous ctx deadline), and the captured stdout stays bounded — never a
// hang, never unbounded buffering.
func TestRunClaude_OutputCapKillsGroup(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	// Flood >cap to stdout, then hang: only the overflow-kill (not the 10s ctx
	// deadline) can end this run quickly.
	bin := writeFakeBin(t, "#!/bin/sh\nyes | head -c 100000\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	stdout, _, _, err := runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "")
	elapsed := time.Since(start)

	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("err = %v, want errOutputTooLarge", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runClaude took %v — the overflow kill did not end the flood promptly", elapsed)
	}
	if len(stdout) > maxChildOutput {
		t.Fatalf("captured stdout = %d bytes, want <= cap %d (must not buffer unbounded)", len(stdout), maxChildOutput)
	}
}

// TestRunClaude_OutputCapBothStreams floods stdout AND stderr past the cap
// concurrently, so the race detector exercises both copy goroutines driving the
// shared kill-group / sync.Once path (the stdout-only test never does). Asserts a
// clean OUTPUT_TOO_LARGE outcome with both captures bounded.
func TestRunClaude_OutputCapBothStreams(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	script := "#!/bin/sh\n" +
		"{ yes | head -c 100000; } &\n" +
		"{ yes | head -c 100000 1>&2; } &\n" +
		"sleep 30\n"
	bin := writeFakeBin(t, script)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	stdout, stderr, _, err := runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "")
	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("err = %v, want errOutputTooLarge", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("both-stream flood not ended promptly by the overflow kill")
	}
	if len(stdout) > maxChildOutput || len(stderr) > maxChildOutput {
		t.Fatalf("capture exceeded cap: stdout=%d stderr=%d cap=%d", len(stdout), len(stderr), maxChildOutput)
	}
}

// TestRun_OutputTooLarge_Surfaces: end-to-end, a vendor child that floods stdout
// past the cap surfaces as SUBAGENT_OUTPUT_TOO_LARGE — not a misclassified
// SUBAGENT_FAILED from a truncated envelope.
func TestRun_OutputTooLarge_Surfaces(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Fake claude: answer --version for ResolveBinaryPath, else flood >cap then hang.
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in --version) echo \"2.1.150\"; exit 0;; esac\n" +
		"yes | head -c 100000\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "vendors.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, Timeout: 10 * time.Second})
	if res.OK {
		t.Fatal("OK should be false on output overflow")
	}
	if res.ErrorCode != ErrCodeOutputTooLarge {
		t.Fatalf("ErrorCode = %s (msg=%s), want SUBAGENT_OUTPUT_TOO_LARGE", res.ErrorCode, res.ErrorMsg)
	}
}
