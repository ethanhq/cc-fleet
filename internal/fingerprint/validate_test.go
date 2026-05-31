package fingerprint

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestValidateForRuntime_NilFP covers the "nil cache" branch — a freshly
// constructed pointer-typed return value from Load that the caller forgot to
// check must not blow up downstream.
func TestValidateForRuntime_NilFP(t *testing.T) {
	err := ValidateForRuntime(nil)
	if err == nil {
		t.Fatal("ValidateForRuntime(nil): want error, got nil")
	}
	if !errors.Is(err, ErrFingerprintStale) {
		t.Fatalf("err = %v, want wrapped ErrFingerprintStale", err)
	}
}

// TestValidateForRuntime_EmptyBinaryPath covers the corrupt-cache branch —
// JSON parse succeeded but binary_path is "" (e.g. an old test fixture or a
// manually edited file).
func TestValidateForRuntime_EmptyBinaryPath(t *testing.T) {
	err := ValidateForRuntime(&Fingerprint{BinaryPath: ""})
	if err == nil {
		t.Fatal("ValidateForRuntime(empty BinaryPath): want error, got nil")
	}
	if !errors.Is(err, ErrFingerprintStale) {
		t.Fatalf("err = %v, want wrapped ErrFingerprintStale", err)
	}
}

// TestValidateForRuntime_BinaryGone covers the CC-upgrade branch — the cache
// names a versioned binary path the upgrade swept away. The test exercises a
// fully constructed fingerprint that only fails on os.Stat.
func TestValidateForRuntime_BinaryGone(t *testing.T) {
	dir := t.TempDir()
	// Build a path under dir that doesn't exist.
	gone := filepath.Join(dir, "no-such-claude")
	err := ValidateForRuntime(&Fingerprint{
		CCVersion:  "1.0.0",
		BinaryPath: gone,
	})
	if err == nil {
		t.Fatal("ValidateForRuntime(missing binary): want error, got nil")
	}
	if !errors.Is(err, ErrFingerprintStale) {
		t.Fatalf("err = %v, want wrapped ErrFingerprintStale", err)
	}
}

// TestValidateForRuntime_Healthy is the inverse — a fingerprint pointing at a
// real on-disk file must pass.
func TestValidateForRuntime_Healthy(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	err := ValidateForRuntime(&Fingerprint{
		CCVersion:  "1.0.0",
		BinaryPath: bin,
	})
	if err != nil {
		t.Fatalf("ValidateForRuntime(healthy): unexpected err %v", err)
	}
}
