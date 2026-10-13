package fileutil

import (
	"os"
	"time"
)

// renameAttempts / renameDelay bound the retry of a replace that windows refuses while another
// handle has the target open: Go (and a virus scanner) opens files without FILE_SHARE_DELETE, so
// MoveFileEx fails until that handle closes. ~0.5s outlasts a reader's open; past it the error is
// returned unchanged.
const (
	renameAttempts = 50
	renameDelay    = 10 * time.Millisecond
)

// renameRetry is os.Rename hardened for AtomicWrite's replace. On unix isRenameBlocked is always
// false, so it is exactly one os.Rename — no retry, no behavior change.
func renameRetry(from, to string) error {
	return retryTransientRename(func() error { return os.Rename(from, to) }, isRenameBlocked, renameAttempts, renameDelay)
}

// retryTransientRename calls rename until it succeeds, fails non-transiently, or attempts is
// spent, sleeping delay between tries. Split from renameRetry so the retry loop is exercisable
// off-platform with an injected rename/predicate.
func retryTransientRename(rename func() error, transient func(error) bool, attempts int, delay time.Duration) error {
	err := rename()
	for i := 1; i < attempts && err != nil && transient(err); i++ {
		time.Sleep(delay)
		err = rename()
	}
	return err
}
