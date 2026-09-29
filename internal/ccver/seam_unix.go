//go:build !windows

package ccver

import "os"

// claudeBinName is the basename of the claude executable in the per-version
// layout: bare `claude` on unix.
const claudeBinName = "claude"

// flatVersionFiles reports whether versions/<semver> may itself be the claude
// binary (the flat per-version layout). Unix only: on windows the executable
// needs its .exe extension, which a bare <semver> name lacks.
const flatVersionFiles = true

// homeForLayout returns the home directory rooting the per-version layout.
// $HOME on unix — read directly so tests that t.Setenv("HOME", ...) stay
// hermetic.
func homeForLayout() string {
	return os.Getenv("HOME")
}

// isExecutableFile reports whether fi (from os.Stat, so a symlink is already
// followed) describes a non-empty regular file with at least one execute bit
// set. A 0-byte file is rejected: the updater leaves one behind mid-download.
func isExecutableFile(fi os.FileInfo) bool {
	if fi == nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
