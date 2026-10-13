//go:build !windows

package fileutil

// isRenameBlocked is always false on unix: rename replaces the target even while another
// handle has it open, so renameRetry collapses to a single os.Rename.
func isRenameBlocked(error) bool { return false }
