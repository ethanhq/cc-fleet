//go:build !windows

package subagent

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// errProcGone is the platform "process/group already gone" sentinel. On unix it
// is ESRCH (no such process), returned by kill when the target has exited.
var errProcGone = syscall.ESRCH

// setGroupAttr makes the child its own process-group leader, so signalling -pid
// reaches the whole tree (claude's Bash-tool grandchildren included). Used by
// both the sync (runClaude) and background (launchBackground) paths.
func setGroupAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// procGroup is the SYNC path's process-group controller. On unix the kernel
// process group (Setpgid) IS the group, keyed by the leader pid, so every
// operation routes through the negative-pid group signal and the struct holds
// no extra state — it exists only to give the windows port a place to hang its
// Job Object handle.
type procGroup struct{}

// newProcGroup returns the controller for one runClaude invocation.
func newProcGroup() *procGroup { return &procGroup{} }

// afterStart is the assign-after-Start hook the windows port uses to bind the
// just-started leader to its Job Object. On unix the group already exists by
// virtue of Setpgid at exec time, so this is a no-op.
func (g *procGroup) afterStart(cmd *exec.Cmd) {}

// signalGroupTerm sends SIGTERM to the whole process group (-pid). A group that
// is already gone (ESRCH) is reported as nil so an exit/deadline race is not
// mistaken for a Cancel failure.
func (g *procGroup) signalGroupTerm(pid int) error {
	if e := syscall.Kill(-pid, syscall.SIGTERM); e != nil && !errors.Is(e, errProcGone) {
		return e
	}
	return nil
}

// killGroupHard SIGKILLs the whole process group (-pid). Best-effort: an empty
// group (ESRCH) is fine.
func (g *procGroup) killGroupHard(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// close releases the controller's resources. The unix procGroup holds no handle
// (the kernel process group needs no cleanup), so this is a no-op; it exists so
// runClaude's `defer pg.close()` compiles identically on both platforms.
func (g *procGroup) close() {}

// killProcessTree is the BACKGROUND path's same-process cleanup reaper: SIGTERM
// to the group, a short grace, then SIGKILL to survivors. It is deliberately
// job-handle-free (the background child is detached after a successful launch
// and must outlive the launcher), so it only ever runs while the just-started
// child is still owned by this process.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	// Negative pid → group. The leader has Setpgid:true, so its pid == pgid.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	// Probe + escalate. ESRCH means the group is gone — leave it.
	if err := syscall.Kill(-pid, 0); err == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// pidAlive reports whether pid is alive via kill(pid, 0): nil → alive; EPERM →
// alive but not ours; ESRCH → gone.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
