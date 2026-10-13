package fileutil

import (
	"errors"
	"testing"
	"time"
)

// TestRetryTransientRenameLoop exercises the retry core off-platform: unix's isRenameBlocked is
// always false, so the production wrapper never loops here.
func TestRetryTransientRenameLoop(t *testing.T) {
	transient := errors.New("access denied")
	isTransient := func(err error) bool { return errors.Is(err, transient) }

	t.Run("retries then succeeds", func(t *testing.T) {
		calls := 0
		err := retryTransientRename(func() error {
			calls++
			if calls < 3 {
				return transient
			}
			return nil
		}, isTransient, 5, time.Millisecond)
		if err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
		if calls != 3 {
			t.Fatalf("calls=%d, want 3 (two retries)", calls)
		}
	})

	t.Run("non-transient returns immediately", func(t *testing.T) {
		hard := errors.New("is a directory")
		calls := 0
		err := retryTransientRename(func() error {
			calls++
			return hard
		}, isTransient, 5, time.Millisecond)
		if !errors.Is(err, hard) {
			t.Fatalf("err=%v, want %v", err, hard)
		}
		if calls != 1 {
			t.Fatalf("calls=%d, want 1 (no retry)", calls)
		}
	})

	t.Run("exhausted returns the transient error", func(t *testing.T) {
		calls := 0
		err := retryTransientRename(func() error {
			calls++
			return transient
		}, isTransient, 5, time.Millisecond)
		if !errors.Is(err, transient) {
			t.Fatalf("err=%v, want %v", err, transient)
		}
		if calls != 5 {
			t.Fatalf("calls=%d, want 5 (the attempt bound)", calls)
		}
	})
}
