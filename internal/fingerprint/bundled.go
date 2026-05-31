package fingerprint

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
)

// BundledVersion is the Claude Code version the embedded default recipe was
// captured from. Spawn uses it as the gate for the post-spawn settle check: a
// running CC newer than the recipe's cc_version MAY carry a drifted spawn
// recipe, so only then is the extra liveness check worth its latency. Bump this
// (and re-capture default_fingerprint.json) each release.
const BundledVersion = "2.1.150"

//go:embed default_fingerprint.json
var bundledJSON []byte

// bundledFingerprint parses the embedded default recipe. The recipe (env +
// flag template) is a real capture shipped inside the binary so a fresh
// install spawns without the skill-orchestrated probe. Its binary_path is
// intentionally empty: callers resolve the live path via
// ccver.Detect at spawn time, so the bundled recipe never pins a
// version-specific path that a CC upgrade would strand.
func bundledFingerprint() (*Fingerprint, error) {
	var fp Fingerprint
	if err := json.Unmarshal(bundledJSON, &fp); err != nil {
		return nil, fmt.Errorf("fingerprint: parse bundled default: %w", err)
	}
	return &fp, nil
}

// LoadOrBundled returns the user's cached fingerprint recipe if present, else
// the embedded bundled default. Unlike Load it never returns ErrNotFound: a
// fresh install (no ~/.config/cc-fleet/fingerprint.json) transparently falls
// back to the bundled recipe, so the first spawn works out of the box with no
// FINGERPRINT_MISSING ceremony.
//
// The returned fingerprint carries only the RECIPE (env + flag template +
// cc_version). Callers MUST resolve the binary path separately (ccver.Detect)
// and overwrite BinaryPath — the stored path is ignored, which is exactly what
// stops a CC upgrade (old versions/<x>/ GC'd) from stranding the cache.
//
// A real I/O or parse error against an EXISTING file is surfaced (not masked
// by the bundled fallback) so a corrupt cache is still visible to the user.
func LoadOrBundled() (*Fingerprint, error) {
	fp, err := Load()
	if err == nil {
		return fp, nil
	}
	if errors.Is(err, ErrNotFound) {
		return bundledFingerprint()
	}
	return nil, err
}
