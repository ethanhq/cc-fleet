//go:build !windows

package teardown

import "os"

// removeTeamDir deletes the team directory. Unix can unlink the still-open team
// lock file, so the removal always completes in place and is never deferred.
func removeTeamDir(dir string) (deferred bool, err error) {
	return false, os.RemoveAll(dir)
}
