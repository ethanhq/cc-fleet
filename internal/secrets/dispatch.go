// Package secrets dispatches vendor API-key lookups across pluggable backends.
//
// Keyget is invoked from Claude Code's apiKeyHelper, so it must keep the key
// bytes off of disk, environment, and logs: the caller (cc-fleet keyget
// command) writes the result to stdout exactly once and exits. Nothing in this
// package may log the key bytes themselves. (Round-robin rotation persists a
// small monotonic counter to <vendor>.rotation — that integer is NOT a key, so
// keeping it on disk does not weaken this contract.)
package secrets

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os/exec"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// ErrNoEnabledKey is returned by the file backend when a vendor has no enabled
// API key to hand out (empty key set, or every entry disabled). keyget surfaces
// it without writing any key bytes.
var ErrNoEnabledKey = errors.New("no enabled API key")

// Keyget resolves the API key for vendor by looking up its vendors.toml entry
// and delegating to the configured secret backend.
//
// The returned bytes have trailing CR/LF stripped so the caller can write them
// to stdout verbatim. The key is never logged.
func Keyget(vendor string) ([]byte, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("keyget %s: load config: %w", vendor, err)
	}

	v, ok := cfg.Vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("keyget %s: unknown vendor (not in vendors.toml)", vendor)
	}

	switch v.SecretBackend {
	case "file":
		return keygetFile(vendor, v)
	case "pass":
		return runBackend(vendor, "pass", "pass", "show", v.SecretRef)
	case "1password":
		// secret_ref is an op secret reference URI (op://vault/item/field),
		// passed verbatim — `op read` does its own parsing.
		return runBackend(vendor, "1password", "op", "read", v.SecretRef)
	case "vault":
		return keygetVault(vendor, v)
	case "keyring":
		return keygetKeyring(vendor, v)
	default:
		return nil, fmt.Errorf("keyget %s: unknown backend %q (want file|pass|1password|vault|keyring)",
			vendor, v.SecretBackend)
	}
}

// keygetVault resolves the vault backend. The secret_ref encodes the KV path
// and field as "<path>#<field>", split on the LAST '#' so a '#' inside the path
// survives; both halves must be non-empty. We then run
// `vault kv get -field=<field> <path>` and return its stdout. A malformed ref
// is a clean classified error (never a panic) and — because parsing happens
// before any exec — can carry no key bytes.
func keygetVault(vendor string, v *config.Vendor) ([]byte, error) {
	path, field, err := parseVaultRef(v.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("keyget %s (vault): %w", vendor, err)
	}
	return runBackend(vendor, "vault", "vault", "kv", "get", "-field="+field, path)
}

// parseVaultRef splits "<path>#<field>" on the last '#'. Both sides must be
// non-empty. The error names neither a key (none has been fetched) nor the ref.
func parseVaultRef(ref string) (path, field string, err error) {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return "", "", errors.New(`secret_ref must be "<path>#<field>"`)
	}
	path, field = ref[:i], ref[i+1:]
	if path == "" || field == "" {
		return "", "", errors.New(`secret_ref must be "<path>#<field>" with a non-empty path and field`)
	}
	return path, field, nil
}

// keygetKeyring resolves the keyring backend via libsecret's secret-tool. The
// secret_ref encodes the lookup attributes as whitespace-separated
// "<attr> <value> ..." pairs, so it must split into a non-zero, even number of
// tokens; we then run `secret-tool lookup <attr> <value> ...`. A malformed ref
// (empty or odd token count) is a clean classified error, never a panic, and
// carries no key.
func keygetKeyring(vendor string, v *config.Vendor) ([]byte, error) {
	attrs, err := parseKeyringRef(v.SecretRef)
	if err != nil {
		return nil, fmt.Errorf("keyget %s (keyring): %w", vendor, err)
	}
	return runBackend(vendor, "keyring", "secret-tool", append([]string{"lookup"}, attrs...)...)
}

// parseKeyringRef tokenizes the ref on whitespace into attribute/value pairs.
// secret-tool lookup needs an even number of arguments (attr value attr value
// ...); an empty ref or an odd token count is rejected.
func parseKeyringRef(ref string) ([]string, error) {
	fields := strings.Fields(ref)
	if len(fields) == 0 {
		return nil, errors.New(`secret_ref must be "<attr> <value> ..." (attribute/value pairs)`)
	}
	if len(fields)%2 != 0 {
		return nil, errors.New(`secret_ref must have an even number of "<attr> <value>" tokens`)
	}
	return fields, nil
}

// keygetFile resolves the file backend's key: load the (possibly multi-)key
// set, keep only the enabled entries, and pick one per the vendor's rotation
// strategy. Disabled keys are filtered out BEFORE selection so they can never
// be handed out.
func keygetFile(vendor string, v *config.Vendor) ([]byte, error) {
	ks, err := LoadKeySet(vendor)
	if err != nil {
		return nil, fmt.Errorf("keyget %s (file): %w", vendor, err)
	}
	enabled := make([]KeyEntry, 0, len(ks))
	for _, e := range ks {
		if e.Enabled {
			enabled = append(enabled, e)
		}
	}
	return selectKey(vendor, v.KeyRotation, enabled)
}

// selectKey chooses one key from the already-filtered enabled set per rotation:
//
//   - off:         always the first enabled key (deterministic; the legacy
//     single-key behavior is unchanged).
//   - round_robin: cycle via the persistent flock-guarded counter.
//   - random:      a uniformly random enabled key (load spreading only).
//
// With a single enabled key all strategies collapse to that key (and
// round_robin does NOT touch the counter). An empty set is ErrNoEnabledKey. The
// returned bytes have trailing CR/LF stripped, matching the historical read.
//
// Rotation is dispatched via config.ParseKeyRotation (typed enum) so an
// unrecognized value surfaces explicitly rather than silently falling through to
// a default — defense-in-depth, since config.Load already validated a
// well-formed vendors.toml.
func selectKey(vendor, rotation string, enabled []KeyEntry) ([]byte, error) {
	if len(enabled) == 0 {
		return nil, fmt.Errorf("keyget %s: %w", vendor, ErrNoEnabledKey)
	}
	if len(enabled) == 1 {
		return keyBytes(enabled[0]), nil
	}
	strategy, err := config.ParseKeyRotation(rotation)
	if err != nil {
		return nil, fmt.Errorf("keyget %s: %w", vendor, err)
	}
	switch strategy {
	case config.RotationOff:
		return keyBytes(enabled[0]), nil
	case config.RotationRoundRobin:
		idx, err := nextRoundRobinIndex(vendor, len(enabled))
		if err != nil {
			return nil, fmt.Errorf("keyget %s: %w", vendor, err)
		}
		return keyBytes(enabled[idx]), nil
	case config.RotationRandom:
		return keyBytes(enabled[rand.IntN(len(enabled))]), nil
	}
	// Unreachable: ParseKeyRotation returns one of the three constants on
	// success. Returning an explicit error keeps the linter happy AND
	// guarantees that adding a new KeyRotation constant without updating
	// this switch produces a hard error instead of a silent off fallback.
	return nil, fmt.Errorf("keyget %s: unhandled key_rotation %q", vendor, strategy)
}

// keyBytes returns an entry's key with trailing CR/LF stripped.
func keyBytes(e KeyEntry) []byte {
	return bytes.TrimRight([]byte(e.Key), "\r\n")
}

// runBackend executes a CLI secret tool (pass / op / vault / secret-tool) and
// returns its trimmed stdout. backendLabel is the short tag used in error
// messages (e.g. "pass").
func runBackend(vendor, backendLabel, command string, args ...string) ([]byte, error) {
	out, err := exec.Command(command, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("keyget %s (%s): %w", vendor, backendLabel, err)
	}
	return bytes.TrimRight(out, "\r\n"), nil
}
