package subagent

import (
	"os"
	"time"
)

// metaReadAttempts / metaReadDelay bound the meta read's retry of a transient Windows
// share/lock violation. AtomicWrite renames the meta into place (MoveFileEx replace); for
// the instant the replace holds the target, a concurrent reader's open fails with
// ERROR_SHARING_VIOLATION. ~5 tries over ~12ms outlast that window; past it the error is
// returned unchanged.
const (
	metaReadAttempts = 5
	metaReadDelay    = 3 * time.Millisecond
)

// readFileRetry is os.ReadFile hardened for the meta read path. On unix isSharingViolation
// is always false, so it is exactly one os.ReadFile — no retry, no behavior change. On
// windows it re-tries only the transient rename-window sharing violation. It never masks a
// torn read: AtomicWrite renames whole files, so a reader observes whole-old or whole-new,
// and a genuinely corrupt/partial meta still surfaces (as a non-transient error or an
// unmarshal failure) for the caller to reject.
func readFileRetry(path string) ([]byte, error) {
	return retryTransientRead(func() ([]byte, error) { return os.ReadFile(path) }, isSharingViolation, metaReadAttempts, metaReadDelay)
}

// retryTransientRead calls read until it succeeds, fails non-transiently, or attempts is
// spent, sleeping delay between tries. Split from readFileRetry so the retry loop is
// exercisable off-platform with an injected read/predicate.
func retryTransientRead(read func() ([]byte, error), transient func(error) bool, attempts int, delay time.Duration) ([]byte, error) {
	data, err := read()
	for i := 1; i < attempts && err != nil && transient(err); i++ {
		time.Sleep(delay)
		data, err = read()
	}
	return data, err
}
