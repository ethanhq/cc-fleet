package secrets

// keyset.go implements the file-backend multi-key store: a vendor can hold
// several API keys in <SecretsDir>/<vendor>.keys.json, each independently
// enabled/disabled, with a per-worker rotation strategy chosen in vendors.toml.
//
// Key-safety: key bytes live only in keys.json (0600, gitignored), in process
// memory, and on keyget's stdout. Nothing here logs a key, and parse errors
// wrap ONLY the encoding/json error (which carries structural/position info,
// never the offending field bytes) — see LoadKeySet.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// KeyEntry is one API key inside a vendor's multi-key store. The JSON tags are
// the public on-disk schema: scripts/tests may hand-write keys.json.
//
// Label is a human-readable name shown in the TUI (empty renders as "keyN");
// it is NOT a secret. Key is the secret. Enabled gates per-key selection.
type KeyEntry struct {
	Label   string `json:"label"`
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// safeVendorName rejects a vendor name that could escape SecretsDir when used to
// build a per-vendor file path. Registered vendors are regex-validated at Add
// time (userops.ValidateVendorName); this is defense-in-depth so any direct
// caller of the keyset API can never turn a name like "../../x" into a read
// outside the secrets dir. The error names only the vendor (never a key).
func safeVendorName(vendor string) error {
	if vendor == "" || vendor == "." || vendor == ".." ||
		strings.ContainsAny(vendor, `/\`) || strings.Contains(vendor, "..") {
		return fmt.Errorf("keyset: invalid vendor name %q", vendor)
	}
	return nil
}

// SafeRef rejects a file-backend secret_ref that could escape SecretsDir when
// joined onto it. A file-backend ref must name a single flat file *inside* the
// secrets dir, so a path separator, a ".."/"." component, or an absolute path
// is refused. It is the secret_ref analogue of safeVendorName and is enforced
// on every file-backend read/write path (userops.writeFileSecret /
// removeFileSecret / Add / Edit and loadLegacyKeySet below) so a hand-edited
// vendors.toml can never turn a ref like "../../etc/shadow" into a read or
// write outside the secrets dir.
//
// It applies ONLY to the file backend: pass / 1password / vault / keyring refs
// legitimately contain "/" (e.g. "secret/data/x", "op://vault/item/field") and
// are never used to build a SecretsDir path. The error names neither the ref
// nor any key — the ref is a filename, not a secret, but the message is kept
// content-free for consistency with the no-leak discipline.
func SafeRef(ref string) error {
	if ref == "" || ref == "." || ref == ".." ||
		strings.ContainsAny(ref, `/\`) || strings.Contains(ref, "..") {
		return errors.New(`secret_ref must be a flat filename inside the secrets dir (no '/', '\', '..', or absolute path)`)
	}
	return nil
}

// keysJSONPath returns <SecretsDir>/<vendor>.keys.json. The name is derived
// from the vendor (not its secret_ref) so the TUI/keyget always know it.
func keysJSONPath(vendor string) (string, error) {
	if err := safeVendorName(vendor); err != nil {
		return "", err
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, vendor+".keys.json"), nil
}

// rotationPath returns <SecretsDir>/<vendor>.rotation — the round-robin
// counter file. Its content is a single decimal integer (NOT a secret).
func rotationPath(vendor string) (string, error) {
	if err := safeVendorName(vendor); err != nil {
		return "", err
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, vendor+".rotation"), nil
}

// LoadKeySet resolves a vendor's key set by priority:
//
//  1. <vendor>.keys.json exists  -> parse it (multi-key mode; authoritative).
//  2. else the legacy secret_ref file exists -> one enabled entry from it.
//  3. else -> empty set (keyget then reports "no enabled API key").
//
// A keys.json that fails to parse is a hard error (we do NOT silently fall back
// to the legacy file — that could hand out the wrong key). The error wraps ONLY
// the json error, never the file bytes or any KeyEntry (key-safety).
func LoadKeySet(vendor string) ([]KeyEntry, error) {
	kp, err := keysJSONPath(vendor)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(kp)
	switch {
	case err == nil:
		var ks []KeyEntry
		if jErr := json.Unmarshal(data, &ks); jErr != nil {
			// Wrap only the json error; do NOT include data or any entry.
			return nil, fmt.Errorf("keyset %s: parse %s: %w", vendor, kp, jErr)
		}
		if ks == nil {
			ks = []KeyEntry{}
		}
		return ks, nil
	case errors.Is(err, os.ErrNotExist):
		// fall through to the legacy single-key path
	default:
		return nil, fmt.Errorf("keyset %s: read %s: %w", vendor, kp, err)
	}

	return loadLegacyKeySet(vendor)
}

// loadLegacyKeySet synthesizes a one-entry key set from the vendor's legacy
// secret_ref file (the pre-multi-key layout). A missing file yields an empty
// set rather than an error so a freshly-added vendor with no key on disk still
// surfaces as "no enabled key" at selection time.
func loadLegacyKeySet(vendor string) ([]KeyEntry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("keyset %s: load config: %w", vendor, err)
	}
	v, ok := cfg.Vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("keyset %s: unknown vendor (not in vendors.toml)", vendor)
	}
	if v.SecretRef == "" {
		return []KeyEntry{}, nil
	}
	if err := SafeRef(v.SecretRef); err != nil {
		return nil, fmt.Errorf("keyset %s: %w", vendor, err)
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, v.SecretRef))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []KeyEntry{}, nil
		}
		return nil, fmt.Errorf("keyset %s: read legacy secret: %w", vendor, err)
	}
	// Trim trailing CR/LF, matching the file-backend read.
	return []KeyEntry{{Label: "key1", Key: string(bytes.TrimRight(data, "\r\n")), Enabled: true}}, nil
}

// SaveKeySet atomically writes ks to <vendor>.keys.json (0600, secrets dir
// 0700). Writing here is what migrates a vendor from legacy single-key to
// multi-key mode: the caller seeds ks[0] from LoadKeySet (which returns the
// legacy key) and appends the new entries before saving.
//
// A nil/empty ks is persisted as "[]" (an explicit empty store), never "null".
func SaveKeySet(vendor string, ks []KeyEntry) error {
	if ks == nil {
		ks = []KeyEntry{}
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keyset %s: mkdir %s: %w", vendor, dir, err)
	}
	kp, err := keysJSONPath(vendor)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		return fmt.Errorf("keyset %s: marshal: %w", vendor, err)
	}
	data = append(data, '\n')
	return fileutil.AtomicWrite(kp, data, 0o600)
}

// IsMultiKey reports whether vendor is in multi-key mode (a <vendor>.keys.json
// exists). Used to guard the CLI `edit --api-key` path, which only manages the
// legacy single key.
func IsMultiKey(vendor string) (bool, error) {
	kp, err := keysJSONPath(vendor)
	if err != nil {
		return false, err
	}
	switch _, statErr := os.Stat(kp); {
	case statErr == nil:
		return true, nil
	case errors.Is(statErr, os.ErrNotExist):
		return false, nil
	default:
		return false, statErr
	}
}

// RemoveKeySet best-effort deletes a vendor's multi-key store and rotation
// counter (<vendor>.keys.json + <vendor>.rotation). A missing file is not an
// error — this is idempotent cleanup invoked from userops.Remove. It returns a
// non-nil error only on a real removal failure (e.g. permissions).
func RemoveKeySet(vendor string) error {
	kp, err := keysJSONPath(vendor)
	if err != nil {
		return err
	}
	rp, err := rotationPath(vendor)
	if err != nil {
		return err
	}
	for _, p := range []string{kp, rp} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// MaskKey renders a key for display so the full secret never reaches the screen
// or a log. Keys of length >= 8 show the first and last 3 runes (e.g.
// "sk-…238"); shorter keys (including empty) are fully obscured by bullets (at
// least 3) so neither the middle nor any prefix/suffix leaks.
func MaskKey(key string) string {
	r := []rune(key)
	if len(r) >= 8 {
		return string(r[:3]) + "…" + string(r[len(r)-3:])
	}
	n := len(r)
	if n < 3 {
		n = 3
	}
	return strings.Repeat("•", n)
}

// nextRoundRobinIndex returns the next index in [0,n) for round-robin selection
// and atomically advances the persistent counter. n must be > 0.
//
// The counter lives in <vendor>.rotation and is guarded by a blocking exclusive
// flock (LOCK_EX) so concurrent cc-fleet processes (each a separate spawn) take
// turns rather than racing. A missing or corrupt counter self-heals to 0.
func nextRoundRobinIndex(vendor string, n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("rotation %s: n must be > 0, got %d", vendor, n)
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return 0, err
	}
	// Create the secrets dir (0700) so a fresh HOME can still rotate.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("rotation %s: mkdir %s: %w", vendor, dir, err)
	}
	path, err := rotationPath(vendor)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, fmt.Errorf("rotation %s: open %s: %w", vendor, path, err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return 0, fmt.Errorf("rotation %s: flock %s: %w", vendor, path, err)
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()

	data, err := io.ReadAll(f)
	if err != nil {
		return 0, fmt.Errorf("rotation %s: read %s: %w", vendor, path, err)
	}
	c := parseCounter(data)
	idx := c % n
	if err := writeCounter(f, c+1); err != nil {
		return 0, fmt.Errorf("rotation %s: %w", vendor, err)
	}
	return idx, nil
}

// parseCounter reads the monotonic counter from the rotation file. An empty,
// non-numeric, or negative value is treated as 0 (self-healing).
func parseCounter(data []byte) int {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0
	}
	c, err := strconv.Atoi(s)
	if err != nil || c < 0 {
		return 0
	}
	return c
}

// writeCounter rewrites f's entire content with the decimal value c.
func writeCounter(f *os.File, c int) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(c)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}
