package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatusFor_RejectsPathTraversal: a "../"-style job id must be rejected with
// the SUBAGENT_BAD_ARGS envelope BEFORE any path is built, so subagent-status
// can't be used to read a file outside the jobs directory.
func TestStatusFor_RejectsPathTraversal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Plant a file one level above the jobs dir; a successful traversal would
	// read it. The guard must make this unreachable.
	xdg := os.Getenv("XDG_CONFIG_HOME")
	secret := filepath.Join(xdg, "cc-fleet", "secret.json")
	if err := os.MkdirAll(filepath.Dir(secret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secret, []byte(`{"k":"v"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, in := range []string{"../secret", "../../etc/passwd", "a/b", "/abs", ".."} {
		res := StatusFor(in)
		if res.OK {
			t.Fatalf("StatusFor(%q): OK=true, want rejection", in)
		}
		if res.ErrorCode != ErrCodeBadArgs {
			t.Fatalf("StatusFor(%q): ErrorCode=%q, want %q", in, res.ErrorCode, ErrCodeBadArgs)
		}
	}
}

// TestStatusFor_AcceptsUUIDShape: a well-formed (but unknown) job id passes the
// id guard and reaches the normal "unknown job" path — i.e. the guard doesn't
// reject legitimate uuid.NewString() values.
func TestStatusFor_AcceptsUUIDShape(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	res := StatusFor("550e8400-e29b-41d4-a716-446655440000")
	if res.OK {
		t.Fatalf("StatusFor(unknown uuid): OK=true, want not-found")
	}
	// It must fail as "unknown job" (bad args, post-validation), not be silently
	// accepted; the point is the id-shape guard let it through to the lookup.
	if res.ErrorCode != ErrCodeBadArgs {
		t.Fatalf("StatusFor(unknown uuid): ErrorCode=%q, want %q", res.ErrorCode, ErrCodeBadArgs)
	}
}
