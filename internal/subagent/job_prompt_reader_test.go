package subagent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// failingReader returns partial bytes then an error, simulating a network /
// piped stdin failure mid-stream. launchBackground must catch the error BEFORE
// calling cmd.Start so claude never receives a partial prompt.
type failingReader struct {
	partial []byte
	err     error
	read    bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	n := copy(p, r.partial)
	return n, r.err
}

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
	writeMinimalVendors(t, xdg)

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

	res := Run(Request{
		Vendor:       "glm",
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

// TestMaterializePromptReader_NilReader: r==nil returns (nil, nil) so the
// caller can route the "no prompt" path without conditional logic.
func TestMaterializePromptReader_NilReader(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prompt")
	f, err := materializePromptReader(nil, dst)
	if err != nil {
		t.Fatalf("materializePromptReader(nil): %v", err)
	}
	if f != nil {
		t.Fatalf("materializePromptReader(nil) returned non-nil file %p", f)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("materializePromptReader(nil) left a file at %s (err=%v)", dst, err)
	}
}

// TestMaterializePromptReader_HappyPath: a working io.Reader is fully drained
// to dst (0o600) and an open *os.File is returned.
func TestMaterializePromptReader_HappyPath(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prompt")
	want := []byte("hello world")
	r := strings.NewReader(string(want))
	f, err := materializePromptReader(r, dst)
	if err != nil {
		t.Fatalf("materializePromptReader: %v", err)
	}
	if f == nil {
		t.Fatal("materializePromptReader returned nil file on success")
	}
	t.Cleanup(func() { f.Close() })

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("dst = %q, want %q", got, want)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("dst mode = %o, want 0o600", st.Mode().Perm())
	}
}

// TestMaterializePromptReader_ReadErrorCleansUp: a reader that errors must
// produce a clean error AND leave no partial file on disk.
func TestMaterializePromptReader_ReadErrorCleansUp(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prompt")
	r := &failingReader{partial: []byte("partial"), err: errors.New("read fail")}
	f, err := materializePromptReader(r, dst)
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("materializePromptReader: want error from failing reader, got nil")
	}
	// io.ReadAll surfaces the wrapped error; the path is left untouched (no
	// partial write happened because os.WriteFile was never called).
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("materializePromptReader left a file at %s after read error (stat err=%v)",
			dst, statErr)
	}
}

// TestMaterializePromptReader_WriteErrorCleansUp: a WriteFile failure must leave
// no partial file at dst (the helper's "On any failure dst is removed
// best-effort" contract).
//
// Forcing os.WriteFile to fail is awkward without a seam; the most reliable
// approach is to pre-create dst as an empty directory. WriteFile then returns
// EISDIR ("is a directory"), and the deferred cleanup must remove the directory
// so the path is gone when the function returns.
func TestMaterializePromptReader_WriteErrorCleansUp(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prompt")
	// Pre-create dst as an empty directory; os.WriteFile against a directory
	// path fails with EISDIR.
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatalf("seed dst as a directory: %v", err)
	}

	r := strings.NewReader("hello world")
	f, err := materializePromptReader(r, dst)
	if err == nil {
		if f != nil {
			f.Close()
		}
		t.Fatal("materializePromptReader: want error when dst is an existing directory, got nil")
	}
	if !strings.Contains(err.Error(), "write prompt") {
		t.Errorf("error mentions stage? got %q, want it to contain \"write prompt\"", err.Error())
	}

	// The defer must have removed the directory so dst is gone.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("materializePromptReader left an artifact at %s after WriteFile error (stat err=%v)",
			dst, statErr)
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
	writeMinimalVendors(t, xdg)

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

	res := Run(Request{
		Vendor:       "glm",
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

// Compile-time guard: failingReader satisfies io.Reader.
var _ io.Reader = (*failingReader)(nil)
