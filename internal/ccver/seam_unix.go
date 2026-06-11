//go:build !windows

package ccver

import "os"

// claudeBinName is the basename of the claude executable in the per-version
// layout: bare `claude` on unix.
const claudeBinName = "claude"

// homeForLayout returns the home directory rooting the per-version layout.
// $HOME on unix — read directly so tests that t.Setenv("HOME", ...) stay
// hermetic.
func homeForLayout() string {
	return os.Getenv("HOME")
}

// isExecutableFile reports whether path is a regular file with at least one
// execute bit set. Used by locate() so a versioned install dir only counts when
// it holds a runnable claude; on unix executability is a file-mode bit.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
