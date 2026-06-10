//go:build !windows

package subagent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// TestLaunchBackground_PromptReaderError_FailsBeforeStart:
//
//  1. Run with a non-*os.File PromptReader whose Read returns partial data + an
//     error.
//  2. Expect the call to fail with SUBAGENT_FAILED and "materialize prompt".
//  3. Verify the fake claude binary was NEVER invoked (no argv log entries) —
//     proving the error path fires before cmd.Start.
//  4. Verify no orphan .out / .err / .prompt files were left behind.
//
// Sync (subagent.Run) is unaffected — it inherits stdin directly and never
// reaches the materialize path.
func TestLaunchBackground_PromptReaderError_FailsBeforeStart(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	// Fake claude that records every invocation to argv.log. If launchBackground
	// regresses and cmd.Start runs anyway, this log will be non-empty.
	argsLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("CCF_ARGS_LOG", argsLog)
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$CCF_ARGS_LOG"; done
exit 0
`
	fakeClaude := writeFakeBin(t, script)
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	reader := &failingReader{
		partial: []byte("partial prompt bytes"),
		err:     errors.New("forced: pipe closed"),
	}

	res := Run(context.Background(), Request{
		Provider:     "glm",
		PromptReader: reader,
		Background:   true,
	})

	if res.OK {
		t.Fatalf("Run(background) with failing reader should fail; got OK=true")
	}
	if res.ErrorCode != ErrCodeFailed {
		t.Errorf("ErrorCode = %q, want SUBAGENT_FAILED", res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMsg, "materialize prompt") {
		t.Errorf("ErrorMsg = %q, want it to mention \"materialize prompt\"", res.ErrorMsg)
	}

	// The fake claude must NEVER have been invoked. If launchBackground ran
	// cmd.Start anyway, the script above would write at least one line to
	// argsLog.
	if data, err := os.ReadFile(argsLog); err == nil && len(data) > 0 {
		t.Fatalf("fake claude was invoked despite materialize failure; argv log = %q",
			string(data))
	}

	// No orphan job artifacts. (.out / .err are created BEFORE the reader
	// check, and the cleanup path also removes the .prompt file.)
	jobsBase := filepath.Join(xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(jobsBase)
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if ext == ".out" || ext == ".err" || ext == ".prompt" {
			t.Errorf("orphan job artifact left behind: %s", name)
		}
	}
}

// TestLaunchBackground_MaterializeWriteError_RemovesPromptFile: even when the
// helper does NOT clean dst itself (we swap in a stub that intentionally leaves
// a partial pf), the caller in launchBackground must `os.Remove(pf)` so the
// materialize-error path leaves no .prompt artifact.
//
//  1. Stub materializePromptFn to write a partial pf + return an error.
//  2. Run with a non-*os.File PromptReader so the materialize branch executes.
//  3. Expect SUBAGENT_FAILED + "materialize prompt" + fake claude UN-invoked.
//  4. Verify no .out / .err / .prompt artifact remains in the jobs dir.
//
// Without the caller's `_ = os.Remove(pf)` the partial .prompt file from step 1
// survives, failing step 4.
func TestLaunchBackground_MaterializeWriteError_RemovesPromptFile(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	// Fake claude that logs every argv. If launchBackground regresses and runs
	// cmd.Start anyway, this would be non-empty.
	argsLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("CCF_ARGS_LOG", argsLog)
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$CCF_ARGS_LOG"; done
exit 0
`
	fakeClaude := writeFakeBin(t, script)
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	// Swap the materializer for a stub that simulates a buggy helper: it writes
	// a partial file to dst (so a real artifact exists) and then returns an
	// error WITHOUT cleaning up. The test then asserts the caller's
	// os.Remove(pf) line removes that artifact.
	origMat := materializePromptFn
	materializePromptFn = func(r io.Reader, dst string) (*os.File, error) {
		// Consume the reader so the test stub mimics a partial write path.
		_, _ = io.ReadAll(r)
		_ = os.WriteFile(dst, []byte("PARTIAL_BYTES_LEFT_BY_BUGGY_HELPER"), 0o600)
		return nil, errors.New("simulated write failure")
	}
	t.Cleanup(func() { materializePromptFn = origMat })

	res := Run(context.Background(), Request{
		Provider:     "glm",
		PromptReader: strings.NewReader("any prompt body; reader type is not *os.File"),
		Background:   true,
	})

	if res.OK {
		t.Fatalf("Run(background) with write-error stub should fail; got OK=true")
	}
	if res.ErrorCode != ErrCodeFailed {
		t.Errorf("ErrorCode = %q, want SUBAGENT_FAILED", res.ErrorCode)
	}
	if !strings.Contains(res.ErrorMsg, "materialize prompt") {
		t.Errorf("ErrorMsg = %q, want it to contain \"materialize prompt\"", res.ErrorMsg)
	}

	// The fake claude must NEVER have been invoked.
	if data, rerr := os.ReadFile(argsLog); rerr == nil && len(data) > 0 {
		t.Fatalf("fake claude was invoked despite materialize failure; argv log = %q",
			string(data))
	}

	// No .out / .err / .prompt artifact may remain. The stub explicitly
	// created a partial .prompt; verifying it is gone proves the caller's
	// _ = os.Remove(pf) ran.
	jobsBase := filepath.Join(xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(jobsBase)
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if ext == ".out" || ext == ".err" || ext == ".prompt" {
			t.Errorf("orphan job artifact left behind after caller cleanup: %s", name)
		}
	}
}
