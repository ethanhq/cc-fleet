package fingerprint

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSemverGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"2.1.152", "2.1.150", true},
		{"2.1.150", "2.1.150", false},
		{"2.1.150", "2.1.152", false},
		{"3.0.0", "2.9.9", true},
		{"2.1.150.1", "2.1.150", true}, // extra component → newer
		{"2.1.150", "2.1.150.1", false},
		{"", "2.1.150", false},      // empty → not greater
		{"2.1.150", "", false},      // empty → not greater
		{"x.y.z", "2.1.150", false}, // unparseable → not greater
	}
	for _, c := range cases {
		if got := semverGreater(c.a, c.b); got != c.want {
			t.Errorf("semverGreater(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestResolveBinaryPath_KeepsExisting: a cached path that still exists on disk
// is returned verbatim (no needless live lookup; keeps test fakes + matched
// recipe/binary working).
func TestResolveBinaryPath_KeepsExisting(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveBinaryPath(&Fingerprint{BinaryPath: bin})
	if err != nil {
		t.Fatalf("ResolveBinaryPath: %v", err)
	}
	if got != bin {
		t.Fatalf("kept path = %q, want %q", got, bin)
	}
}

// TestResolveBinaryPath_StalePathFallsBack: a cached path that no longer
// exists (post-upgrade GC) must NOT be returned — it falls back to a live
// lookup. We can't guarantee a claude install in CI, so assert only that the
// dead path is never echoed back (either a live path or a STALE error).
func TestResolveBinaryPath_StalePathFallsBack(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "versions", "0.0.0", "claude") // never created
	got, err := ResolveBinaryPath(&Fingerprint{BinaryPath: dead})
	// We can't guarantee a claude install in CI, so a STALE error is acceptable.
	// But a nil error MUST mean a real, existing binary was resolved — never the
	// dead path, and never an empty/non-existent path with nil error.
	if err == nil {
		if got == dead {
			t.Fatalf("stale path was returned verbatim instead of falling back: %q", got)
		}
		if got == "" {
			t.Fatal("nil error but empty path — must resolve to a real binary")
		}
		if _, statErr := os.Stat(got); statErr != nil {
			t.Fatalf("resolved path does not exist on disk: %q (%v)", got, statErr)
		}
	}
}

// TestResolveBinaryPath_EmptyPathResolvesLive: a bundled recipe (empty path)
// must resolve to a live binary or return STALE — never empty-with-nil-error.
func TestResolveBinaryPath_EmptyPathResolvesLive(t *testing.T) {
	got, err := ResolveBinaryPath(&Fingerprint{BinaryPath: ""})
	// A nil error must mean a real binary was resolved live; otherwise STALE.
	if err == nil {
		if got == "" {
			t.Fatal("empty path returned empty with nil error — must resolve live or error")
		}
		if _, statErr := os.Stat(got); statErr != nil {
			t.Fatalf("resolved path does not exist on disk: %q (%v)", got, statErr)
		}
	}
}
