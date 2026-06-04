//go:build windows

package teardown

// reapTeammateProcess is a no-op on Windows. The teammate/spawn lane is
// unsupported there (teardown is reachable but the spawn command fails clean
// before launching any process), so there is no ghost teammate process to reap;
// it returns no killed pids and no warnings.
func reapTeammateProcess(agentID string) (killed []int, warnings []string) {
	return nil, nil
}
