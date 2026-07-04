//go:build windows

package subagent

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isSharingViolation reports the transient contention a concurrent AtomicWrite rename
// (MoveFileEx replace) raises against a reader's open — SHARING for the file handle,
// LOCK for a byte range. Both clear once the replace completes, so readFileRetry re-tries
// them; every other error (including ErrNotExist) is returned immediately.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
