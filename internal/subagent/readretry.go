package subagent

import (
	"os"
	"time"
)

// metaReadAttempts / metaReadDelay bound the retry of a transient Windows share/lock violation on a
// meta OR run-manifest read. AtomicWrite renames the file into place (MoveFileEx replace); for the
// instant the replace holds the target, a concurrent reader's open fails with ERROR_SHARING_VIOLATION.
// ~5 tries over ~12ms outlast that window; past it the error is returned unchanged.
const (
	metaReadAttempts = 5
	metaReadDelay    = 3 * time.Millisecond
)

// osReadFile / transientRead are readFileRetry's read primitive and its transient-error predicate,
// held as package vars only so a test can drive the retry loop off-platform: unix's isSharingViolation
// is always false, so a Windows sharing violation can't otherwise be simulated on the meta/manifest
// read path. Production never reassigns them.
var (
	osReadFile    = os.ReadFile
	transientRead = isSharingViolation
)

// readFileRetry is os.ReadFile hardened for the meta + run-manifest read paths. On unix
// isSharingViolation is always false, so it is exactly one os.ReadFile — no retry, no behavior
// change. On windows it re-tries only the transient rename-window sharing violation. It never masks a
// torn read: AtomicWrite renames whole files, so a reader observes whole-old or whole-new, and a
// genuinely corrupt/partial file still surfaces (as a non-transient error or an unmarshal failure)
// for the caller to reject.
func readFileRetry(path string) ([]byte, error) {
	return retryTransientRead(func() ([]byte, error) { return osReadFile(path) }, transientRead, metaReadAttempts, metaReadDelay)
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
