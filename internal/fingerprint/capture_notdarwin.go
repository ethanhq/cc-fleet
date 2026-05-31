//go:build !darwin

package fingerprint

import "errors"

// captureFromPidDarwin is never called off darwin (CaptureFromPid guards on
// runtime.GOOS == "darwin"), but must exist so the non-darwin build compiles the
// reference in capture.go. On Linux the /proc-file path (CaptureFromFiles) is
// used instead; on other platforms refresh-fingerprint has no live-probe path.
func captureFromPidDarwin(pid int) (*Fingerprint, error) {
	return nil, errors.New("fingerprint: darwin ps capture not available on this platform")
}
