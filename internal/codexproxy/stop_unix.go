//go:build !windows

package codexproxy

import "os"

// stopProcess is the fallback when an authenticated /shutdown is unavailable: send
// the daemon SIGINT (Serve installs no signal handler, so the default kills it).
func stopProcess(pid int) {
	if pid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(os.Interrupt)
	}
}
