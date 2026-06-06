package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// detachedReaper owns a launched detached engine's lifecycle handles: its wait4 (which
// reaps the child so it never lingers as a `<defunct>` zombie under a long-lived parent
// like the TUI board) and the /dev/null fd kept open for the child's stdio. EXACTLY ONE
// path drives it: the success path runs wait() in a goroutine (fire-and-forget reaper);
// the startup-timeout path kill()s then wait()s synchronously so the pid is truly gone
// before the run is failed. They are mutually exclusive owners of cmd.Wait.
type detachedReaper struct {
	cmd     *exec.Cmd
	devnull *os.File
}

func (r *detachedReaper) wait() error {
	werr := r.cmd.Wait()
	r.devnull.Close()
	return werr
}

func (r *detachedReaper) kill() { _ = r.cmd.Process.Kill() }

// launchDetached re-execs cc-fleet as a detached `workflow run --foreground --run-id`
// child that runs the engine to completion after the launching CLI exits, so the main
// session is never blocked for the run's duration (a fan-out easily outlasts a single
// Bash-call timeout). It reuses the subagent leaf's process-group primitive
// (SetDetachGroup), no new platform code. The child's stdio goes to /dev/null (the
// detached engine keeps no stderr log); its observable state is the manifest + the
// live-event channel + board, and the engine's top-level recover finalizes status even
// on a panic. It returns the child pid + a detachedReaper the caller MUST drive (instead
// of Process.Release, which leaves a zombie under a long-lived parent): on success
// `go reaper.wait()`; on a startup timeout `reaper.kill()` then `reaper.wait()`.
func launchDetached(scriptPath, runID string, opts Options) (int, *detachedReaper, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, nil, fmt.Errorf("workflow: locate cc-fleet binary: %w", err)
	}
	argv := []string{"workflow", "run", scriptPath, "--foreground", "--run-id", runID}
	if opts.Concurrency > 0 {
		argv = append(argv, "--max-concurrency", strconv.Itoa(opts.Concurrency))
	}
	if opts.ArgsJSON != "" {
		argv = append(argv, "--args-json", opts.ArgsJSON)
	}
	if opts.NoPersistIO {
		argv = append(argv, "--no-persist-io")
	}
	if opts.BudgetUSD > 0 {
		argv = append(argv, "--budget-usd", strconv.FormatFloat(opts.BudgetUSD, 'f', -1, 64))
	}
	if opts.BudgetTokens > 0 {
		argv = append(argv, "--budget-tokens", strconv.FormatInt(opts.BudgetTokens, 10))
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, nil, fmt.Errorf("workflow: open %s: %w", os.DevNull, err)
	}

	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	// Scrub the detached engine's env of the lead's creds + nested-CC/teams markers, the
	// same boundary a subagent child gets — so a long-lived detached cc-fleet process never
	// holds ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN in its environ. (The leaves re-clean
	// their own claude child env regardless; this protects the engine process itself.)
	cmd.Env = childenv.Clean(os.Environ())
	subagent.SetDetachGroup(cmd)
	if err := cmd.Start(); err != nil {
		devnull.Close()
		return 0, nil, fmt.Errorf("workflow: start detached run: %w", err)
	}
	return cmd.Process.Pid, &detachedReaper{cmd: cmd, devnull: devnull}, nil
}
