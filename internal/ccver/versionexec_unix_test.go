//go:build !windows

package ccver

import (
	"path/filepath"
	"testing"
	"time"
)

// TestVersionFromExecShimChildHoldsStdout: a shim whose child outlives it on the
// shared stdout (npm's codex.js) must not hold the probe past its timeout.
func TestVersionFromExecShimChildHoldsStdout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5s version timeout")
	}
	shim := filepath.Join(t.TempDir(), "shim")
	binWriteFile(t, shim, "#!/bin/sh\nsleep 20 &\nexec sleep 20\n", 0o755)

	start := time.Now()
	if v := versionFromExec(shim); v != "" {
		t.Fatalf("version = %q, want \"\"", v)
	}
	if d := time.Since(start); d > versionExecTimeout+3*time.Second {
		t.Fatalf("probe took %v, want it bounded near the %v timeout", d, versionExecTimeout)
	}
}
