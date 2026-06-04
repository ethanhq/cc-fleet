//go:build windows

package secrets

import (
	"os"

	"golang.org/x/sys/windows"
)

// rotLockEx takes a blocking exclusive lock on the first byte of the rotation
// counter file — the Windows analogue of flock(LOCK_EX). Omitting
// LOCKFILE_FAIL_IMMEDIATELY makes LockFileEx block until acquired, so concurrent
// subagents serialize their keyget round-robin increment rather than colliding
// on a key index. A non-zero range is required, so we lock exactly one byte.
func rotLockEx(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,    // reserved
		1, 0, // lock 1 byte (low, high)
		&overlapped,
	)
}

// rotUnlock releases the 1-byte lock taken by rotLockEx. Best-effort; the lock
// is also released when the handle is closed.
func rotUnlock(f *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
