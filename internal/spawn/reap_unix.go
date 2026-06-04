//go:build !windows

package spawn

import (
	"syscall"
	"time"
)

// defaultReapAgentProcess is the production fallback for reapAgentProcess: a
// SIGTERM/grace/SIGKILL cycle against any process whose cmdline matches
// `--agent-id <agentID>`. Best-effort — failure is recorded but never raised.
//
// We deliberately replicate (rather than import) a small slice of
// teardown.reapTeammateProcess to avoid an internal/spawn → internal/teardown
// dependency edge. Teardown still runs its own reap on the normal teardown path;
// this is only the rollback-on-failure cleanup.
func defaultReapAgentProcess(agentID string) {
	if agentID == "" {
		return
	}
	pids := findAgentPIDs(agentID)
	for _, pid := range pids {
		// Keep the grace tight (200ms) so the fail-fast spawn JSON envelope
		// stays timely.
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			continue
		}
		time.Sleep(200 * time.Millisecond)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
