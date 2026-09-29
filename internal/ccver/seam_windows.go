//go:build windows

package ccver

import "os"

// claudeBinName is the basename of the claude executable in the per-version
// layout: `claude.exe` on windows.
const claudeBinName = "claude.exe"

// flatVersionFiles is false on windows: a bare versions/<semver> file has no
// .exe extension, so only versions/<semver>/claude.exe counts.
const flatVersionFiles = false

// homeForLayout returns the home directory rooting the per-version layout.
// %USERPROFILE% on windows, where HOME is not a native variable.
func homeForLayout() string {
	return os.Getenv("USERPROFILE")
}

// isExecutableFile reports whether fi (from os.Stat) describes a non-empty
// regular file. On windows executability is extension-driven (the lookup
// already targets claude.exe) and there is no x bit to check; a 0-byte file is
// rejected because the updater leaves one behind mid-download.
func isExecutableFile(fi os.FileInfo) bool {
	return fi != nil && fi.Mode().IsRegular() && fi.Size() > 0
}
