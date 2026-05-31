// Package fingerprint provides the per-cc-version spawn template (env vars +
// flag list) that cc-fleet replays when launching vendor teammates.
//
// The template ships as a BUNDLED default recipe embedded in the binary (see
// bundled.go's //go:embed default_fingerprint.json, loaded via LoadOrBundled),
// so a fresh install spawns with no first-run probe. A user
// ~/.config/cc-fleet/fingerprint.json — a real capture from a live native Agent
// process (see capture.go) — is an optional OVERRIDE that takes precedence; the
// skill-orchestrated probe only refreshes it on drift, not on every spawn.
//
// This file deals only with the on-disk cache: the Fingerprint struct, atomic
// I/O against ~/.config/cc-fleet/fingerprint.json (0600), and a freshness
// check against the currently installed cc binary version.
package fingerprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// fingerprintFileName is the basename used inside ConfigDir.
const fingerprintFileName = "fingerprint.json"

// ErrNotFound is returned by Load / LoadFromPath when the cache file does not
// exist. Callers (spawn, doctor, refresh-fingerprint) discriminate this from
// real I/O failures via errors.Is.
var ErrNotFound = errors.New("fingerprint not found")

// Fingerprint is the cached spawn recipe for one Claude Code version.
//
// Field names and JSON tags are part of the on-disk schema — do not rename
// without considering migration of existing user caches.
type Fingerprint struct {
	CCVersion     string            `json:"cc_version"`
	CapturedAt    time.Time         `json:"captured_at"`
	BinaryPath    string            `json:"binary_path"`
	Env           map[string]string `json:"env"`
	FlagsTemplate []string          `json:"flags_template"`
}

// Path returns the default cache path: <ConfigDir>/fingerprint.json.
func Path() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fingerprintFileName), nil
}

// Load reads and parses the default fingerprint cache.
//
// A missing file returns ErrNotFound (wrapped) so the skill-orchestrated
// self-heal flow can react without parsing error strings.
func Load() (*Fingerprint, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	return LoadFromPath(path)
}

// LoadFromPath is Load against an explicit path. Used by tests.
func LoadFromPath(path string) (*Fingerprint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("fingerprint: %s: %w", path, ErrNotFound)
		}
		return nil, fmt.Errorf("fingerprint: read %s: %w", path, err)
	}
	var fp Fingerprint
	if err := json.Unmarshal(data, &fp); err != nil {
		return nil, fmt.Errorf("fingerprint: parse %s: %w", path, err)
	}
	return &fp, nil
}

// Save writes fp to the default cache path atomically with mode 0600.
func Save(fp *Fingerprint) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return SaveToPath(fp, path)
}

// SaveToPath writes fp to path atomically (CreateTemp + rename) with mode
// 0600. The parent directory is created on demand at mode 0700.
func SaveToPath(fp *Fingerprint, path string) error {
	if fp == nil {
		return errors.New("fingerprint: nil Fingerprint")
	}

	data, err := json.MarshalIndent(fp, "", "  ")
	if err != nil {
		return fmt.Errorf("fingerprint: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fingerprint: mkdir %s: %w", dir, err)
	}

	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("fingerprint: write %s: %w", path, err)
	}
	return nil
}

// IsStale reports whether fp's cached cc_version differs from the currently
// installed one. A nil fp or empty currentCCVersion is treated as stale so
// callers default to "go re-probe".
func IsStale(fp *Fingerprint, currentCCVersion string) bool {
	if fp == nil {
		return true
	}
	if currentCCVersion == "" {
		return true
	}
	return fp.CCVersion != currentCCVersion
}
