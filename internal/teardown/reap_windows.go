//go:build windows

package teardown

import "errors"

// reapProcess is unsupported on Windows: the teammate lane refuses there
// (teardown reports UNSUPPORTED_ON_WINDOWS before discovery).
func reapProcess(pid int, start string) error {
	return errors.New("process reaping is not supported on Windows")
}
