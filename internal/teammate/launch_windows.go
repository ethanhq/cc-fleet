//go:build windows

package teammate

import (
	"fmt"
	"io"
)

// launchMain refuses on Windows: the teammate lane needs tmux panes and the
// POSIX shim, neither of which exists there.
func launchMain(args []string, _, stderr io.Writer) int {
	vals, _ := scanLaunchArgv(args, nil)
	fmt.Fprintln(stderr, FormatFailureLine(CodeUnsupportedOnWindows, failureAgentID(vals[flagAgentID]),
		"provider teammates are not supported on Windows", "use `cc-fleet subagent` or `cc-fleet workflow`"))
	return 1
}
