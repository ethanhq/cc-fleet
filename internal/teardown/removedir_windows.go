//go:build windows

package teardown

import "os"

// removeTeamDir deletes the team directory. Windows refuses to unlink the team
// lock file while WithTeamLock still holds it open, so a failure here is
// reported as deferred rather than fatal: the caller retries once the lock has
// been released. A dir that is already gone is success, not a deferral.
func removeTeamDir(dir string) (deferred bool, err error) {
	if err := os.RemoveAll(dir); err != nil {
		if _, statErr := os.Stat(dir); os.IsNotExist(statErr) {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}
