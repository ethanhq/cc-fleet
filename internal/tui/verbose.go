package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// openVerboseLog creates the TUI's --verbose sink: <ConfigDir>/tui-verbose-<pid>.log,
// 0600, truncated. Per-pid names keep concurrent verbose sessions from truncating
// each other; the sweep below bounds accumulation.
func openVerboseLog() (*os.File, string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	sweepStaleVerboseLogs(dir, os.Getpid())
	path := filepath.Join(dir, fmt.Sprintf("tui-verbose-%d.log", os.Getpid()))
	// Always write to a fresh private inode. This pid's own file is never swept
	// (and a pre-reboot pid's may linger), so the predictable path can already
	// exist — possibly a symlink or a broader-mode file. Remove it first, then
	// O_EXCL-create: a symlink is unlinked (not followed into its target), and a
	// retained reader of the old inode keeps the orphaned file, never our writes.
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, path, nil
}

// sweepStaleVerboseLogs removes tui-verbose-<pid>.log siblings whose embedded pid
// is no longer alive — sequential use leaves exactly one file, while a live
// concurrent session's file (its printed path must stay inspectable) is never
// touched. Best-effort: parse and remove failures are ignored.
func sweepStaleVerboseLogs(dir string, selfPID int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "tui-verbose-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "tui-verbose-"), ".log"))
		if err != nil || pid <= 0 || pid == selfPID || pidAliveForSweep(pid) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}
