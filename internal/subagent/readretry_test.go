package subagent

import (
	"errors"
	"testing"
	"time"
)

// TestRetryTransientReadLoop exercises the retry core off-platform (unix's isSharingViolation
// is always false, so the production wrapper never loops here): a transient error retries up to
// the attempt bound, a non-transient error returns at once, and an exhausted retry returns the
// last transient error — the shape the windows meta read relies on.
func TestRetryTransientReadLoop(t *testing.T) {
	transient := errors.New("sharing violation")
	isTransient := func(err error) bool { return errors.Is(err, transient) }

	t.Run("retries then succeeds", func(t *testing.T) {
		calls := 0
		data, err := retryTransientRead(func() ([]byte, error) {
			calls++
			if calls < 3 {
				return nil, transient
			}
			return []byte("ok"), nil
		}, isTransient, 5, time.Millisecond)
		if err != nil || string(data) != "ok" {
			t.Fatalf("data=%q err=%v, want ok/nil", data, err)
		}
		if calls != 3 {
			t.Fatalf("calls=%d, want 3 (two retries)", calls)
		}
	})

	t.Run("non-transient returns immediately", func(t *testing.T) {
		hard := errors.New("not found")
		calls := 0
		_, err := retryTransientRead(func() ([]byte, error) {
			calls++
			return nil, hard
		}, isTransient, 5, time.Millisecond)
		if !errors.Is(err, hard) {
			t.Fatalf("err=%v, want hard", err)
		}
		if calls != 1 {
			t.Fatalf("calls=%d, want 1 (no retry on a non-transient error)", calls)
		}
	})

	t.Run("gives up after the attempt bound", func(t *testing.T) {
		calls := 0
		_, err := retryTransientRead(func() ([]byte, error) {
			calls++
			return nil, transient
		}, isTransient, 5, time.Millisecond)
		if !errors.Is(err, transient) {
			t.Fatalf("err=%v, want the last transient error", err)
		}
		if calls != 5 {
			t.Fatalf("calls=%d, want 5 (the attempt bound)", calls)
		}
	})
}
