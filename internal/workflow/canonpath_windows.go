//go:build windows

package workflow

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

// canonPath resolves p to the form `git worktree list --porcelain` / `git rev-parse --show-toplevel`
// emit, so a prefix comparison against git output holds on every platform. On Windows git reports the
// LONG path while os.TempDir() may be the 8.3 short form (C:\Users\RUNNER~1\… vs the long
// C:\Users\runneradmin\…); GetLongPathName bridges them (normPath's slash+case fold cannot). A
// resolve error — e.g. p does not exist — falls back to p: a later prefix miss then simply skips,
// never deletes (fail-safe).
func canonPath(p string) string {
	if long, err := longPathName(p); err == nil {
		return long
	}
	return p
}

// longPathName expands p's 8.3 short components to their long form via GetLongPathName, growing the
// buffer on the documented too-small return (required size, incl. NUL, > buflen).
func longPathName(p string) (string, error) {
	u16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	n := uint32(len(p) + 16)
	for {
		buf := make([]uint16, n)
		got, gerr := windows.GetLongPathName(u16, &buf[0], n)
		if gerr != nil {
			return "", gerr
		}
		if got <= n {
			return filepath.Clean(windows.UTF16ToString(buf[:got])), nil
		}
		n = got
	}
}
