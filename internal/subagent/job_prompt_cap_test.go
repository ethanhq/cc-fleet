package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// floodReader yields an endless stream of 'a' so the prompt cap can be exercised
// without allocating a huge source buffer.
type floodReader struct{}

func (floodReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestMaterializePromptReader_Oversized: a prompt past the cap fails BEFORE
// cmd.Start (so the child never gets a partial prompt) and leaves no orphan
// .prompt file behind.
func TestMaterializePromptReader_Oversized(t *testing.T) {
	orig := maxPromptBytes
	maxPromptBytes = 1024
	t.Cleanup(func() { maxPromptBytes = orig })

	dst := filepath.Join(t.TempDir(), "p.prompt")
	f, err := materializePromptReader(floodReader{}, dst)
	if err == nil {
		t.Fatal("oversized prompt: want error, got nil")
	}
	if f != nil {
		t.Fatalf("returned non-nil file %p on error", f)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an 'exceeds' size error", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("oversized prompt left an orphan file at %s (stat err=%v)", dst, statErr)
	}
}
