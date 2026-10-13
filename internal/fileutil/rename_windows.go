//go:build windows

package fileutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isRenameBlocked reports the contention MoveFileEx raises while another handle is in the way
// of the replace — ACCESS_DENIED when the target is open without FILE_SHARE_DELETE, SHARING
// when the temp file is (a scanner); LOCK mirrors the reader side. All clear once that handle
// closes, so renameRetry re-tries them; every other error is returned immediately.
func isRenameBlocked(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
