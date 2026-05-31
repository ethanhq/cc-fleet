// Package fileutil holds small file-system primitives shared across cc-fleet.
//
// AtomicWrite is the single place that implements the create-tempfile + chmod +
// write + close + rename + cleanup discipline used by config / models / profile
// / fingerprint / secrets / spawn / userops for every 0o600 metadata or secret
// file, so a future hardening only changes one implementation.
package fileutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWrite writes data to path via a SAME-DIRECTORY temp file + rename at
// mode. The parent directory must already exist — this helper deliberately does
// not mkdir so callers keep control of the directory mode.
//
// Discipline:
//
//  1. CreateTemp in filepath.Dir(path) so the rename stays within one
//     filesystem (atomic at the kernel level).
//  2. Chmod the temp to exactly mode regardless of umask. CreateTemp's
//     0600-before-umask is not enough.
//  3. Write data, Close, Rename. Any failure jumps to cleanup.
//  4. Defer os.Remove on the temp path so a failure before rename never
//     leaves an orphan; after a successful rename the temp name no longer
//     exists, so the deferred Remove no-ops.
//
// A partial/failed write therefore never truncates an existing file at path —
// the old file stays intact until os.Rename swaps in the fully-written
// replacement in one step. This matters for in-place secret rotation: the
// previous key stays usable rather than clobbered.
//
// The error wraps a short stage tag + path + the underlying error, never data.
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	if path == "" {
		return errors.New("fileutil.AtomicWrite: empty path")
	}
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("fileutil.AtomicWrite: create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Defer cleanup runs on every path: on failure it removes the orphan
	// temp file; after a successful rename the temp name no longer exists,
	// so it no-ops.
	defer func() { _ = os.Remove(tmpPath) }()

	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fileutil.AtomicWrite: chmod temp %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fileutil.AtomicWrite: write temp %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fileutil.AtomicWrite: close temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("fileutil.AtomicWrite: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
