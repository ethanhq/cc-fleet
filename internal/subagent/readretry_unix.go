//go:build !windows

package subagent

// isSharingViolation is always false on unix: os.Rename is atomic on the inode, so a
// concurrent reader never sees the transient replace-window contention Windows reports.
// readFileRetry therefore collapses to a single os.ReadFile — no retry cost.
func isSharingViolation(error) bool { return false }
