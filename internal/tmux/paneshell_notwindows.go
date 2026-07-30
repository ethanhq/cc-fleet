//go:build !windows

package tmux

// wrapPaneCommand is a no-op off Windows: tmux hands the pane command to a POSIX
// shell already, so the command is passed through verbatim.
func wrapPaneCommand(cmd string) string { return cmd }
