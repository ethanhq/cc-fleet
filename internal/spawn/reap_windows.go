//go:build windows

package spawn

// defaultReapAgentProcess is a no-op on Windows. The teammate/spawn lane is
// unsupported there (the spawn command fails clean before any process is
// launched), so the rollback-on-failure reap has nothing to terminate.
func defaultReapAgentProcess(agentID string) {}
