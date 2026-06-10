package config

// keyrotation.go defines the typed KeyRotation enum shared across config /
// secrets / TUI, so the dispatch sites can't disagree on the vocabulary or
// silently map a typo to a default.

import "fmt"

// KeyRotation is the per-worker file-backend multi-key rotation strategy.
//
// Persisted on disk as the underlying string (providers.toml's key_rotation
// field) — the constants below mirror that schema. "" is accepted as an alias
// for RotationOff: older providers.toml files without the key parse into "" and
// must continue to behave like off.
type KeyRotation string

const (
	// RotationOff: always the first enabled key (deterministic; the legacy
	// single-key behavior is unchanged).
	RotationOff KeyRotation = "off"

	// RotationRoundRobin: cycle via the persistent flock-guarded counter
	// (<provider>.rotation in SecretsDir).
	RotationRoundRobin KeyRotation = "round_robin"

	// RotationRandom: uniformly random enabled key per call (load spreading).
	RotationRandom KeyRotation = "random"
)

// ValidKeyRotations returns the canonical valid values for providers.toml's
// key_rotation field, in the order Validate uses for error messages. The
// leading "" is the legacy "unset" form (treated as an alias for "off") so
// pre-field configs still validate; UI callers wanting only user-facing labels
// skip the empty entry.
func ValidKeyRotations() []string {
	return []string{"", "off", "round_robin", "random"}
}

// ParseKeyRotation parses a providers.toml key_rotation value into a typed
// KeyRotation. The empty string is normalized to RotationOff. Any other value
// is an error — the caller cannot fall back to a default (which would
// reintroduce the typo-silently-accepted bug).
//
// Used by selectKey (defense-in-depth — config.Validate already rejects typos
// at load time so this is the second gate) and by the TUI's nextRotation
// cycle.
func ParseKeyRotation(s string) (KeyRotation, error) {
	switch s {
	case "", string(RotationOff):
		return RotationOff, nil
	case string(RotationRoundRobin):
		return RotationRoundRobin, nil
	case string(RotationRandom):
		return RotationRandom, nil
	default:
		return RotationOff, fmt.Errorf("invalid key_rotation %q (want one of %v)",
			s, ValidKeyRotations())
	}
}

// Next returns the next strategy in the canonical UI cycle:
//
//	off -> round_robin -> random -> off
//
// Used by the TUI key-manager's `t` shortcut. Centralized here so the cycle
// can never disagree with the parser.
func (r KeyRotation) Next() KeyRotation {
	switch r {
	case RotationOff:
		return RotationRoundRobin
	case RotationRoundRobin:
		return RotationRandom
	case RotationRandom:
		return RotationOff
	default:
		// Defense-in-depth: an unknown stored value cycles back to Off so a
		// hand-corrupted providers.toml doesn't wedge the TUI rotation key.
		return RotationOff
	}
}
