//go:build !windows

package config

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes a blocking exclusive advisory lock (flock LOCK_EX) on f. No
// LOCK_NB — concurrent holders serialize behind each other rather than error.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

// unlockFile releases the advisory lock. Best-effort; the kernel also releases
// it when f is closed.
func unlockFile(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
