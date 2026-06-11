//go:build windows

package codexproxy

import (
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// liveWorkers counts running claude workers bound to this proxy's port by scanning
// cc-fleet's own job store: process argv is unreadable on windows, so the unix
// argv-scan is replaced by the recorded (status, proxy_port, pid+proc_start) of each
// subagent job. A config-dir error returns 0 — no job is provably bound, and the
// idle-grace timer still gates the exit (a stranded daemon is never the cost of an
// uncertain scan here, unlike the unix table read).
func liveWorkers(port int) int {
	dir, err := config.ConfigDir()
	if err != nil {
		return 0
	}
	return countJobWorkers(filepath.Join(dir, "subagent-jobs"), port, pidAlive)
}
