//go:build windows

package ccver

import "os"

// claudeBinName is the basename of the claude executable in the per-version
// layout: `claude.exe` on windows.
const claudeBinName = "claude.exe"

// homeForLayout returns the home directory rooting the per-version layout.
// %USERPROFILE% on windows, where HOME is not a native variable.
func homeForLayout() string {
	return os.Getenv("USERPROFILE")
}

// isExecutableFile reports whether path is a regular file. Used by locate() so
// a versioned install dir only counts when it holds a runnable claude; on
// windows executability is extension-driven and the basename is already
// claude.exe, so a regular file is enough.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().IsRegular()
}
