//go:build !windows

package teardown

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// Process-reaping seams. A teammate's claude process reparents to init and keeps
// running after its tmux pane is killed (the ghost teammate), so teardown
// explicitly terminates it by agent id. These are package vars so tests can
// substitute them and never signal real processes during `go test`.
var (
	// findTeammatePIDs locates live teammate processes by agent id.
	findTeammatePIDs = discoverTeammatePIDs
	// signalProc sends sig to pid (sig 0 probes liveness).
	signalProc = func(pid int, sig syscall.Signal) error {
		return syscall.Kill(pid, sig)
	}
	// procReapGrace is how long we wait after SIGTERM before escalating to
	// SIGKILL.
	procReapGrace = 750 * time.Millisecond
)

// reapTeammateProcess terminates any claude process still running under agentID:
// SIGTERM first, then SIGKILL after procReapGrace if it's still alive. It is
// best-effort — a process that's already gone is not an error, and signal
// failures are returned as warnings rather than aborting teardown. Returns the
// pids it confirmed killed plus any warnings.
func reapTeammateProcess(agentID string) (killed []int, warnings []string) {
	for _, pid := range findTeammatePIDs(agentID) {
		if err := signalProc(pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue // raced us and exited — nothing to do
			}
			warnings = append(warnings,
				fmt.Sprintf("SIGTERM pid %d (%s): %v", pid, agentID, err))
			continue
		}
		// Give it a moment to exit on SIGTERM, then force-kill survivors.
		time.Sleep(procReapGrace)
		probe := signalProc(pid, 0)
		stillAlive := probe == nil || errors.Is(probe, syscall.EPERM)
		if stillAlive {
			if err := signalProc(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				warnings = append(warnings,
					fmt.Sprintf("SIGKILL pid %d (%s): %v", pid, agentID, err))
				continue
			}
		}
		killed = append(killed, pid)
	}
	return killed, warnings
}
