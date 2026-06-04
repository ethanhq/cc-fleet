//go:build !windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ----- runClaude with fake binaries (unix: the fake claude is a /bin/sh script,
// and the process-group reap asserts via syscall.Kill) -----

func TestRunClaude_SuccessEnvelope(t *testing.T) {
	bin := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, stderr, code, err := runClaude(ctx, bin, []string{bin, "-p", "x"}, os.Environ(), nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// A clean success must report no run error and an empty stderr channel.
	if err != nil {
		t.Fatalf("clean success returned err=%v, want nil", err)
	}
	if len(stderr) != 0 {
		t.Fatalf("clean success wrote to stderr: %q", stderr)
	}
	res := classify(Request{Vendor: "mimo", JSON: true}, "fallback", stdout, nil, code, false, true)
	if !res.OK || res.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("classify of fake success: OK=%v result=%q", res.OK, res.Result)
	}
}

func TestRunClaude_ErrorEnvelopeExit1(t *testing.T) {
	bin := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smoke429BalanceJSON+"'\nexit 1\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, code, err := runClaude(ctx, bin, []string{bin, "-p", "x"}, os.Environ(), nil)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	// A non-zero exit must surface the run error (an *exec.ExitError), not be
	// swallowed — exitCode is derived from it.
	if err == nil {
		t.Fatal("exit-1 returned nil err, want a non-nil run error")
	}
	res := classify(Request{Vendor: "glm", JSON: true}, "glm-4.6", stdout, nil, code, false, true)
	if res.OK || res.ErrorCode != ErrCodeInsufficientBalance {
		t.Fatalf("want INSUFFICIENT_BALANCE from exit-1 envelope, got OK=%v code=%s", res.OK, res.ErrorCode)
	}
}

func TestRunClaude_StdinPrompt(t *testing.T) {
	// Echo back stdin so we prove --prompt-file/stdin actually reaches the child.
	bin := writeFakeBin(t, "#!/bin/sh\ncat\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stdout, _, code, _ := runClaude(ctx, bin, []string{bin, "-p"}, os.Environ(), strings.NewReader("piped-prompt"))
	if code != 0 || string(stdout) != "piped-prompt" {
		t.Fatalf("stdin not piped: code=%d stdout=%q", code, stdout)
	}
}

// TestRunClaude_TimeoutKillsProcessGroup proves a grandchild that IGNORES
// SIGTERM is still reaped (via the escalated SIGKILL to the whole process
// group) when the deadline fires — no orphan survives.
func TestRunClaude_TimeoutKillsProcessGroup(t *testing.T) {
	// Shrink the SIGTERM→SIGKILL grace so the test is fast.
	orig := waitGrace
	waitGrace = 500 * time.Millisecond
	t.Cleanup(func() { waitGrace = orig })

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// Leader and grandchild both trap (ignore) SIGTERM; only a SIGKILL to the
	// group can reap them. The grandchild records its pid so we can assert death.
	script := "#!/bin/sh\n" +
		"trap '' TERM\n" +
		"sh -c 'trap \"\" TERM; echo $$ > \"" + pidFile + "\"; sleep 30' &\n" +
		"sleep 30\n"
	bin := writeFakeBin(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, _, _ = runClaude(ctx, bin, []string{bin}, os.Environ(), nil)
	elapsed := time.Since(start)

	// Should return within deadline + grace + slack, not hang on the pipe-holding
	// grandchild (WaitDelay defeats that).
	if elapsed > 4*time.Second {
		t.Fatalf("runClaude took %v with a pipe-holding grandchild; WaitDelay/kill model broken", elapsed)
	}

	gpid := readPID(t, pidFile)
	if gpid <= 0 {
		t.Fatalf("grandchild never recorded its pid (%q)", pidFile)
	}
	// The grandchild ignored SIGTERM; only the group SIGKILL escalation reaps it.
	if alive := waitGone(gpid, 3*time.Second); alive {
		// Best-effort cleanup so a failure doesn't leak a sleeper.
		_ = syscall.Kill(gpid, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d survived the timeout (orphan) — process-group SIGKILL escalation missing", gpid)
	}
}

// TestRun_NoUserFingerprint_UsesBundled: with NO
// ~/.config/cc-fleet/fingerprint.json, Run must NOT return FINGERPRINT_MISSING —
// LoadOrBundled supplies the embedded recipe and the binary path resolves live.
// A fast-exit fake claude on PATH keeps the run from reaching the (unreachable)
// vendor or launching a real claude.
func TestRun_NoUserFingerprint_UsesBundled(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Fake claude on PATH so ResolveBinaryPath finds a binary and runClaude
	// execs something that exits instantly (never touches the invalid base_url).
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"),
		[]byte("#!/bin/sh\ncase \"$1\" in --version) echo \"2.1.150\";; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Minimal vendors.toml with one enabled vendor.
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

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true})
	// The bundled fallback must engage: no FINGERPRINT_MISSING, and the binary
	// resolved (no FINGERPRINT_STALE either, since the fake claude is on PATH).
	if res.ErrorCode == ErrCodeFingerprintMissing || res.ErrorCode == ErrCodeFingerprintStale {
		t.Fatalf("missing user fingerprint must fall back to bundled recipe, got %s: %s", res.ErrorCode, res.ErrorMsg)
	}
}
