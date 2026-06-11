package main

import (
	"os"
	"testing"
)

// uninstallAllNeedsYes guards the destructive --all path: --json must always
// demand --yes (a prompt or no-envelope abort would corrupt the single-envelope
// stdout contract), and so must a non-interactive stdin (a pipe can't answer).
func TestUninstallAllNeedsYes(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()

	if !uninstallAllNeedsYes(true, r) {
		t.Fatal("--json must always require --yes")
	}
	if !uninstallAllNeedsYes(false, r) {
		t.Fatal("a non-TTY stdin must require --yes")
	}

	// /dev/null is a char device but not a terminal — automation feeding it
	// must hit the --yes requirement, not an exit-0 prompt abort.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()
	if !uninstallAllNeedsYes(false, devnull) {
		t.Fatalf("%s stdin must require --yes", os.DevNull)
	}
}
