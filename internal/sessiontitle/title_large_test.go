package sessiontitle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTranscriptTitle_TailSeekToNewline: the tail chunk must start at the
// nearest preceding '\n' + 1 so every line in the tail is whole. Otherwise a
// custom-title line straddling the fixed (size - tailLen) offset splits into a
// head-half (lost in the gap between head and tail chunks) and a tail-half (json
// unmarshal fails). The test builds a >320 KiB transcript with the custom-title
// line straddling the tail boundary.
//
// Layout (sizes chosen relative to head=64 KiB / tail=256 KiB constants):
//   - prefix padding: harmless noise lines covering byte 0..tailStart-30
//   - target: a 110-byte custom-title line straddling the tail boundary
//     (~30 bytes of it sit in the head-gap, ~80 in the tail)
//   - suffix padding: noise filling the rest of the tail so the file is
//     definitely > head + tail.
func TestReadTranscriptTitle_TailSeekToNewline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	sessionID := "abcd1234-abcd-4abc-8abc-abcdabcdabcd"

	target := fmt.Sprintf(
		`{"type":"custom-title","customTitle":"Recovered","sessionId":%q}`,
		sessionID) + "\n"
	if len(target) >= 256 {
		t.Fatalf("test setup: target line %d bytes is too long for tail-edge straddle", len(target))
	}

	// File needs to exceed head + tail; let's aim for ~head + tail + 100 KiB so
	// neither chunk covers the entire file.
	totalSize := int64(headReadBytes) + int64(tailReadBytes) + 100*1024
	// Target's start sits 30 bytes BEFORE the tail boundary: the first ~30
	// bytes of the target line live in the gap between head and tail (a
	// scan that read only [0..head) and [tailStart..size) would miss them).
	straddleOffset := int64(30)
	tailBoundary := totalSize - int64(tailReadBytes)
	targetStart := tailBoundary - straddleOffset

	if targetStart <= int64(headReadBytes) {
		t.Fatalf("test setup: targetStart=%d must exceed headReadBytes=%d so the head chunk doesn't cover it",
			targetStart, headReadBytes)
	}

	// Build a noise line. We pad with this to fill the prefix and suffix.
	noise := `{"type":"user","message":{"content":"noise"},"sessionId":"` + sessionID + `"}` + "\n"

	var b strings.Builder
	b.Grow(int(totalSize) + 1024)
	for int64(b.Len())+int64(len(noise)) <= targetStart {
		b.WriteString(noise)
	}
	// The last noise line wrote up to byte b.Len() (ending in '\n'). Pad the
	// remaining bytes before targetStart with a single bridging JSONL line so
	// the byte immediately before target is '\n' (otherwise the seek-to-'\n'
	// chunk would prepend non-line bytes to the target and json unmarshal
	// would fail). The bridge line carries a wrong sessionId so the scan
	// ignores it.
	bridgeLen := targetStart - int64(b.Len())
	if bridgeLen <= 0 {
		t.Fatalf("test setup: bridge gap %d <= 0", bridgeLen)
	}
	// Build a bridge of exactly bridgeLen bytes ending in '\n'.
	bridgePrefix := `{"type":"user","sessionId":"ignored","pad":"`
	bridgeSuffix := `"}` + "\n"
	pad := bridgeLen - int64(len(bridgePrefix)+len(bridgeSuffix))
	if pad < 0 {
		t.Fatalf("test setup: bridge prefix+suffix %d > bridgeLen %d",
			len(bridgePrefix)+len(bridgeSuffix), bridgeLen)
	}
	b.WriteString(bridgePrefix)
	b.WriteString(strings.Repeat("a", int(pad)))
	b.WriteString(bridgeSuffix)
	if int64(b.Len()) != targetStart {
		t.Fatalf("test setup: after bridge b.Len()=%d, want targetStart=%d",
			b.Len(), targetStart)
	}
	b.WriteString(target)
	// Fill the rest of the file with noise lines, then any leftover bytes with
	// a final bridge line so the file ends cleanly at totalSize.
	for int64(b.Len())+int64(len(noise)) <= totalSize {
		b.WriteString(noise)
	}
	// Tail bridge: build one final line filling the gap exactly. If the gap
	// is too small to hold even the bridge prefix+suffix, write \n filler.
	tailGap := totalSize - int64(b.Len())
	switch {
	case tailGap <= 0:
		// already filled
	case tailGap < int64(len(bridgePrefix)+len(bridgeSuffix)):
		// Write tailGap-1 'a's + a single '\n'.
		b.WriteString(strings.Repeat("a", int(tailGap-1)))
		b.WriteByte('\n')
	default:
		pad := tailGap - int64(len(bridgePrefix)+len(bridgeSuffix))
		b.WriteString(bridgePrefix)
		b.WriteString(strings.Repeat("a", int(pad)))
		b.WriteString(bridgeSuffix)
	}

	final := b.String()
	if int64(len(final)) <= int64(headReadBytes)+int64(tailReadBytes) {
		t.Fatalf("test setup: file size %d <= head+tail %d", len(final),
			int64(headReadBytes)+int64(tailReadBytes))
	}

	// Verify the target line really straddles the tail boundary (without the
	// fix, tail chunk would miss the prefix bytes of the target line).
	actualTailBoundary := int64(len(final)) - int64(tailReadBytes)
	targetEnd := targetStart + int64(len(target))
	if !(targetStart < actualTailBoundary && actualTailBoundary < targetEnd) {
		t.Fatalf("test setup: target line [%d..%d) does not straddle tail boundary %d (file size %d)",
			targetStart, targetEnd, actualTailBoundary, len(final))
	}

	path := filepath.Join(dir, "projects", "p", sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(final), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := Lookup(sessionID)
	if got != "Recovered" {
		t.Fatalf("Lookup = %q, want %q — tail-chunk seek to '\\n' boundary must include the straddling custom-title line",
			got, "Recovered")
	}
}

// TestSeekToNewline_BoundedLookback: a pathological 8-KiB line should not trigger
// an unbounded reverse scan. With maxLookback=1024 the helper falls back to start
// when no newline is found within the bound, ruling out runaway I/O on a corrupt
// file.
func TestSeekToNewline_BoundedLookback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// 8 KiB of 'A' with no newlines, then the file ends.
	if err := os.WriteFile(path, []byte(strings.Repeat("A", 8192)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	// Start at byte 7000, look back 1024 bytes only — must NOT scan further.
	start := int64(7000)
	got := seekToNewline(f, start, 1024)
	if got != start {
		t.Fatalf("seekToNewline = %d, want fallback to start=%d when no newline within bound",
			got, start)
	}
}

// TestSeekToNewline_FindsNearestNewline: the happy path — there's a newline
// somewhere in the lookback window, and the helper returns the byte just
// after it.
func TestSeekToNewline_FindsNearestNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	body := strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 100) // newline at offset 100
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	// Start at offset 150 (deep into the second line), look back up to 4 KiB:
	// should find the '\n' at offset 100 and return 101.
	got := seekToNewline(f, 150, 4096)
	if got != 101 {
		t.Fatalf("seekToNewline = %d, want 101 (just past the newline at 100)", got)
	}
}

// TestSeekToNewline_StartAtZero: starting at offset 0 must return 0
// immediately (no read).
func TestSeekToNewline_StartAtZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if got := seekToNewline(f, 0, 4096); got != 0 {
		t.Fatalf("seekToNewline(0) = %d, want 0", got)
	}
}
