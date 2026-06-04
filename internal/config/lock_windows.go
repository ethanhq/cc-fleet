//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes a blocking exclusive lock on the first byte of f, the Windows
// analogue of flock(LOCK_EX). LockFileEx WITHOUT LOCKFILE_FAIL_IMMEDIATELY
// blocks until the range is acquired, so concurrent holders serialize rather
// than error (matching the unix LOCK_EX semantics). A non-zero range is
// required — a zero-length range locks nothing — so we lock exactly one byte at
// offset 0.
func lockFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,    // reserved
		1, 0, // lock 1 byte (low, high)
		&overlapped,
	)
}

// unlockFile releases the 1-byte lock taken by lockFile. Best-effort; the lock
// is also released when the handle is closed.
func unlockFile(f *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
