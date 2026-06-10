package main

import (
	"os"

	"github.com/charmbracelet/x/term"

	"github.com/ethanhq/cc-fleet/internal/tui"
)

// shouldEnterTUI decides whether a bare `cc-fleet` invocation should launch
// the interactive TUI. It is a pure function of the parsed positional args and
// the two tty facts so it can be unit-tested without a real terminal.
//
// The TUI launches ONLY when:
//   - no positional args were given (a true bare invocation), and
//   - both stdin and stdout are terminals.
//
// Any pipe, redirect, CI runner, or `cc-fleet </dev/null` therefore keeps the
// existing behavior (cobra prints help) and never blocks on the event loop.
// Subcommands (`add`, `spawn`, …) bypass this entirely — cobra only calls the
// root Run when no subcommand matched.
func shouldEnterTUI(args []string, stdinTTY, stdoutTTY bool) bool {
	return len(args) == 0 && stdinTTY && stdoutTTY
}

// runTUIIfInteractive launches the TUI when shouldEnterTUI allows it. It reports
// handled=true when the TUI ran (so the caller skips printing help) and surfaces
// any tea.Program error.
//
// First-run onboarding (tmux + agent-teams setup screens) lives INSIDE the TUI:
// tui.NewModel opens on a setup screen when needed. Because the TUI is gated here
// to the bare-interactive both-TTY path, spawn/subagent/piped/agent callers never
// see the setup screens.
func runTUIIfInteractive(args []string, verbose bool) (handled bool, err error) {
	stdinTTY := term.IsTerminal(os.Stdin.Fd())
	stdoutTTY := term.IsTerminal(os.Stdout.Fd())
	if !shouldEnterTUI(args, stdinTTY, stdoutTTY) {
		return false, nil
	}
	// Pre-TUI (terminal still in normal mode): offer a once-a-day update if a
	// newer release is cached. Gated to exactly this bare-interactive path, so
	// keyget / subagent / spawn / --json / non-TTY callers never reach it.
	maybePromptUpdate()
	return true, tui.Run(verbose)
}
