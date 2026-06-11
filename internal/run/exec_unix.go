//go:build !windows

package run

import (
	"syscall"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// execClaude replaces the current process with claude via execve, so the daemon
// liveness of a daemon-backed provider is carried by the resident cc-fleet
// process the exec leaves behind — v is unused here. A seam so tests intercept
// the launch instead of replacing the test process.
var execClaude = func(_ *config.Provider, bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}
