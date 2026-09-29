//go:build !windows

package teammate

import "io"

func launchMain(args []string, stdout, stderr io.Writer) int {
	return runLauncher(args, stdout, stderr)
}
