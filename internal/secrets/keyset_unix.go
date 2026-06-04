//go:build !windows

package secrets

import (
	"os"

	"golang.org/x/sys/unix"
)

// rotLockEx takes a blocking exclusive advisory lock (flock LOCK_EX) on the
// rotation counter file so concurrent cc-fleet processes serialize their
// round-robin increment rather than racing. No LOCK_NB.
func rotLockEx(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// rotUnlock releases the advisory lock. Best-effort; the kernel also releases
// it when f is closed.
func rotUnlock(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
