package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Cross-platform materializePromptReader tests. The launchBackground exec cases
// (fake claude via /bin/sh) live in job_prompt_reader_unix_test.go. failingReader
// lives in helpers_test.go.

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
	// NTFS reports 0666; the 0600 contract is unix-only.
	if perm := st.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("dst mode = %o, want 0o600", perm)
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
// an is-a-directory error, and the deferred cleanup must remove the directory
// so the path is gone when the function returns.
func TestMaterializePromptReader_WriteErrorCleansUp(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prompt")
	// Pre-create dst as an empty directory; os.WriteFile against a directory
	// path fails.
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
