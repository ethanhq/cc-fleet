// ValidateForRuntime is the SINGLE gate that checks a loaded *Fingerprint is
// usable to spawn / subagent a vendor binary RIGHT NOW. Both spawn and subagent
// run it BEFORE any state mutation (pane split / team-config write / profile
// write), so a stale fingerprint fails before, not after, a pane is opened. A
// doctor / refresh path can opt out by reading the cache directly — runtime
// callers MUST go through this helper.
package fingerprint

import (
	"errors"
	"fmt"
	"os"
)

// ErrFingerprintStale is the sentinel ValidateForRuntime wraps for every
// failure mode. Callers (spawn / subagent) errors.Is() on this and map to
// FINGERPRINT_STALE in their result envelopes.
//
// Why a single sentinel for "nil" / "empty BinaryPath" / "binary path
// missing": from the caller's POV all three mean "the cached recipe can't be
// replayed right now — re-probe". Splitting the sentinel would force callers
// to map three sub-codes onto one outer FINGERPRINT_STALE, with no actionable
// difference.
var ErrFingerprintStale = errors.New("fingerprint: stale")

// ValidateForRuntime reports an error iff fp is not safely usable for an
// immediate spawn / subagent run. Three checks, all wrapped with
// ErrFingerprintStale so callers can dispatch via errors.Is:
//
//  1. fp != nil  — a nil cache is the same outcome as an empty file from the
//     callers' standpoint.
//  2. fp.BinaryPath != "" — an empty binary path is a corrupt cache; nothing
//     downstream can recover.
//  3. os.Stat(fp.BinaryPath) succeeds — after a CC upgrade the version-pinned
//     binary may have been garbage-collected, leaving a still-on-disk
//     fingerprint that names a path that no longer exists.
//
// The check stops at the first failing rule (rule order: nil → empty path →
// stat). Each rule's error string includes enough context (the bad
// BinaryPath) that the JSON envelope's error_msg is debuggable without
// further log access.
func ValidateForRuntime(fp *Fingerprint) error {
	if fp == nil {
		return fmt.Errorf("%w: nil fingerprint", ErrFingerprintStale)
	}
	if fp.BinaryPath == "" {
		return fmt.Errorf("%w: missing binary_path", ErrFingerprintStale)
	}
	if _, err := os.Stat(fp.BinaryPath); err != nil {
		return fmt.Errorf("%w: binary_path %q not found: %v",
			ErrFingerprintStale, fp.BinaryPath, err)
	}
	return nil
}
