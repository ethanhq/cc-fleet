//go:build !windows

package teardown

import (
	"reflect"
	"syscall"
	"testing"
)

// TestReapRechecksStartToken: the start token is re-checked before SIGTERM and
// again before SIGKILL, so an exited or reused pid is never signalled.
func TestReapRechecksStartToken(t *testing.T) {
	origStart, origSignal, origGrace := procStartFn, signalProc, procReapGrace
	t.Cleanup(func() { procStartFn, signalProc, procReapGrace = origStart, origSignal, origGrace })
	procReapGrace = 0

	cases := []struct {
		name      string
		start     string // token before SIGTERM ("" = process gone)
		afterTerm string // token after SIGTERM
		termErr   error
		want      []syscall.Signal
	}{
		{"same process", "s1", "s1", nil, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}},
		{"exited on SIGTERM", "s1", "", nil, []syscall.Signal{syscall.SIGTERM}},
		{"reused during grace", "s1", "s2", nil, []syscall.Signal{syscall.SIGTERM}},
		{"reused before SIGTERM", "s2", "s2", nil, nil},
		{"gone before SIGTERM", "", "", nil, nil},
		{"ESRCH on SIGTERM", "s1", "s1", syscall.ESRCH, []syscall.Signal{syscall.SIGTERM}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := tc.start
			var got []syscall.Signal
			procStartFn = func(pid int) (string, bool) { return cur, cur != "" }
			signalProc = func(pid int, sig syscall.Signal) error {
				if pid != 42 {
					t.Fatalf("signalled pid %d", pid)
				}
				got = append(got, sig)
				if sig == syscall.SIGTERM {
					cur = tc.afterTerm
					return tc.termErr
				}
				return nil
			}
			if err := reapProcess(42, "s1"); err != nil {
				t.Fatalf("reapProcess: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("signals = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestReapSignalErrorReported: a SIGTERM failure other than ESRCH is returned.
func TestReapSignalErrorReported(t *testing.T) {
	origStart, origSignal := procStartFn, signalProc
	t.Cleanup(func() { procStartFn, signalProc = origStart, origSignal })
	procStartFn = func(int) (string, bool) { return "s1", true }
	signalProc = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := reapProcess(42, "s1"); err == nil {
		t.Fatal("EPERM on SIGTERM: err = nil")
	}
}
