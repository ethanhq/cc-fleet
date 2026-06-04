//go:build windows

package subagent

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// errProcGone is the platform "process already gone" sentinel. Windows has no
// ESRCH; this is used only for API symmetry with the unix seam.
var errProcGone = errors.New("subagent: process gone")

// stillActive is the exit code GetExitCodeProcess reports for a process that is
// still running (STILL_ACTIVE, 259).
const stillActive = 259

// setGroupAttr starts the child in a NEW process group so it does not share the
// parent's Ctrl-C/Ctrl-Break console group. Tree reaping is done via the Job
// Object (sync path) or taskkill /T (background cleanup), not signals, but the
// new group keeps a console Ctrl event from leaking to the parent. Used by both
// the sync and background paths, mirroring the unix Setpgid.
func setGroupAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// procGroup is the SYNC path's process-tree controller. Windows has no process
// groups or signals, so the whole-tree kill is implemented with a Job Object:
// the child (and every descendant it spawns) is assigned to the job, and
// terminating the job kills the entire tree atomically — the race-free analog of
// kill(-pgid). Used only by runClaude, where the launcher (cc-fleet) stays alive
// for the whole run; the detached background path must NOT use a
// KILL_ON_JOB_CLOSE job (closing the handle on launcher exit would kill the
// child it deliberately detached), so it reaps via killProcessTree instead.
type procGroup struct {
	job windows.Handle
}

// newProcGroup creates the Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so
// that terminating or closing the handle reaps every process still in the job.
// A failure to create the job leaves job == 0; killGroupHard then falls back to
// taskkill /T /F so a timeout still tree-kills.
func newProcGroup() *procGroup {
	g := &procGroup{}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return g // job == 0; killGroupHard falls back to taskkill
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return g // job == 0; fall back to taskkill
	}
	g.job = job
	return g
}

// afterStart binds the just-started leader to the Job Object. Go's Windows
// StartProcess closes the primary thread handle before returning, so the
// textbook CREATE_SUSPENDED → assign → ResumeThread sequence cannot be driven
// through exec.Command; instead we assign immediately after Start returns the
// pid. The escape window — between the leader starting and this assign — is
// sub-millisecond and is the contract-equivalent of the tiny window unix already
// tolerates between Start and the first group signal. Best-effort: if the job is
// absent or the open/assign fails, killGroupHard degrades to taskkill /T /F.
func (g *procGroup) afterStart(cmd *exec.Cmd) {
	if g.job == 0 || cmd.Process == nil {
		return
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		// Can't open the leader → the job stays empty; drop it so killGroupHard
		// falls back to taskkill instead of a no-op TerminateJobObject.
		g.close()
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		// Assignment failed → an empty job would make killGroupHard's
		// TerminateJobObject a no-op; drop it so the taskkill fallback runs.
		g.close()
	}
}

// close releases the Job Object handle. Idempotent (g.job is nil'd), so runClaude's
// deferred close is a no-op when killGroupHard already terminated+closed the job on a
// timeout/overflow path. Without it the handle and its kernel job object would leak
// on the normal-exit path — a Windows-only divergence from the handle-free unix
// procGroup, and an unbounded leak if runClaude is ever driven in a loop.
func (g *procGroup) close() {
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
}

// signalGroupTerm is the graceful-termination request. Windows has no group
// SIGTERM; a best-effort taskkill /T (no /F) asks the current tree to close. The
// authoritative reap is killGroupHard (job terminate), so a failure here is
// benign and reported as nil — matching the unix Cancel contract that an
// already-gone group is not an error.
func (g *procGroup) signalGroupTerm(pid int) error {
	_ = exec.Command("taskkill", "/T", "/PID", strconv.Itoa(pid)).Run()
	return nil
}

// killGroupHard kills the whole process tree. Primary: TerminateJobObject, which
// kills every process still in the job atomically (re-parented grandchildren
// included) — the race-free analog of kill(-pgid, SIGKILL). It then closes the
// handle. Fallback when no job was assigned: taskkill /T /F /PID walks the
// current tree.
func (g *procGroup) killGroupHard(pid int) {
	if g.job != 0 {
		_ = windows.TerminateJobObject(g.job, 1)
		_ = windows.CloseHandle(g.job)
		g.job = 0
		return
	}
	taskkillTree(pid, true)
}

// killProcessTree is the BACKGROUND path's same-process cleanup reaper: a
// graceful taskkill /T, a short grace, then a forced taskkill /T /F on any
// survivor. It is deliberately job-handle-free — the background child is
// detached after a successful launch and must outlive the launcher, so a
// KILL_ON_JOB_CLOSE job would wrongly kill it when the launcher exits — and only
// ever runs while the just-started child is still owned by this process. The /T
// walks the current tree (the claude .cmd shim → node child layer), the small
// race window matching the proposal's accepted taskkill fallback.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	taskkillTree(pid, false)
	time.Sleep(200 * time.Millisecond)
	if pidAlive(pid) {
		taskkillTree(pid, true)
	}
}

// taskkillTree runs taskkill on the pid's whole tree (/T). force adds /F.
func taskkillTree(pid int, force bool) {
	args := []string{"/T", "/PID", strconv.Itoa(pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	_ = exec.Command("taskkill", args...).Run()
}

// pidAlive reports whether pid is alive: OpenProcess(QUERY_LIMITED_INFORMATION)
// then GetExitCodeProcess, treating STILL_ACTIVE (259) as alive. A pid that can
// be opened but has exited reports its real exit code (not 259) → dead; a pid
// that cannot be opened is treated as gone. Replaces the unix kill(pid, 0).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
