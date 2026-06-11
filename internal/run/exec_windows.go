package run

import (
	"context"
	"os"
	"os/exec"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
)

// execClaude launches claude as a child and waits, since Windows has no execve.
// stdio is passed through; no CREATE_NEW_PROCESS_GROUP, so a console Ctrl-C
// reaches the child. While the child runs, a daemon-backed provider's conversion
// daemon is held alive by KeepAlive (a no-op for native providers) in a goroutine
// cancelled on child exit. The "on success never returns" contract is kept by
// exiting with the child's code: a clean child → os.Exit(0); a nonzero exit →
// os.Exit(that code); only a failure to start returns to the caller. A seam so
// tests intercept the launch instead of spawning a real process.
var execClaude = func(v *config.Provider, bin string, argv, env []string) error {
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go codexproxy.KeepAlive(ctx, v)

	err := cmd.Wait()
	cancel()
	if exitErr, ok := err.(*exec.ExitError); ok {
		os.Exit(exitErr.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil // unreachable
}
