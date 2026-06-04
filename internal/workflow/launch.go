package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// launchDetached re-execs cc-fleet as a detached `workflow run --foreground --run-id`
// child that runs the engine to completion after the launching CLI exits, so the main
// session is never blocked for the run's duration (a fan-out easily outlasts a single
// Bash-call timeout). It reuses the subagent leaf's process-group primitive
// (SetDetachGroup) + Process.Release — the same proven detach, no new platform code.
// The child's stdio goes to /dev/null (v1 does not persist a narrator log); the
// manifest + board carry the run's observable state, and the engine's top-level
// recover finalizes status even on a panic.
func launchDetached(scriptPath, runID string, opts Options) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("workflow: locate cc-fleet binary: %w", err)
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
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("workflow: open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()

	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	// Scrub the detached engine's env of the lead's creds + nested-CC/teams markers, the
	// same boundary a subagent child gets — so a long-lived detached cc-fleet process never
	// holds ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN in its environ. (The leaves re-clean
	// their own claude child env regardless; this protects the engine process itself.)
	cmd.Env = childenv.Clean(os.Environ())
	subagent.SetDetachGroup(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("workflow: start detached run: %w", err)
	}
	// Detach: stop tracking the child so this process can exit cleanly while it runs on.
	return cmd.Process.Release()
}
