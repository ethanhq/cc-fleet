package main

import (
	"fmt"
	"os"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/run"
)

// interactiveTTY reports whether both stdin and stdout are terminals — the
// precondition for an interactive REPL. A var so tests can stub it; mirrors the
// TUI gate (shouldEnterTUI also requires both).
var interactiveTTY = func() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func newRunCmd() *cobra.Command {
	var (
		model          string
		permissionMode string
		dangerSkip     bool
	)
	cmd := &cobra.Command{
		Use:   "run <vendor> [-- <claude args>]",
		Short: "Launch an interactive claude session backed by a vendor",
		Long: "Replace this process with an interactive `claude` REPL whose LLM backend is the\n" +
			"named vendor: its profile pins the apiKeyHelper + base URL, and the model is the\n" +
			"vendor's default_model unless --model overrides. The vendor key never enters env,\n" +
			"argv, or shell history. Requires an interactive terminal; Unix/macOS only.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			vendor, extra, err := splitRunArgs(args, cmd.ArgsLenAtDash())
			if err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet run:", err)
				os.Exit(2)
			}
			permMode, err := resolvePermissionOverride(permissionMode, dangerSkip)
			if err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet run:", err)
				os.Exit(2)
			}
			if !interactiveTTY() {
				fmt.Fprintln(os.Stderr, "cc-fleet run: stdin and stdout must both be a terminal for an interactive session")
				os.Exit(1)
			}
			if err := run.Run(run.Request{Vendor: vendor, Model: model, PermissionMode: permMode, ExtraArgs: extra}); err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet run:", err)
				os.Exit(1)
			}
			return nil // unreachable: a successful run.Run replaced the process
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "Vendor model id (default: the vendor's default_model)")
	cmd.Flags().StringVar(&permissionMode, "permission-mode", "",
		"Permission mode for the session (default|acceptEdits|plan|auto|bypassPermissions)")
	cmd.Flags().BoolVar(&dangerSkip, "dangerously-skip-permissions", false,
		"Skip permission prompts (alias for --permission-mode bypassPermissions)")
	return cmd
}

// splitRunArgs requires exactly one vendor positional before any "--", and
// returns the post-"--" tokens as claude passthrough. dashIdx is
// cobra Command.ArgsLenAtDash() (-1 when no "--" was given). Pure, so the arg
// contract is unit-tested without running the command.
func splitRunArgs(args []string, dashIdx int) (vendor string, extra []string, err error) {
	if dashIdx < 0 {
		if len(args) != 1 {
			return "", nil, fmt.Errorf("usage: cc-fleet run <vendor> [-- <claude args>]")
		}
		return args[0], nil, nil
	}
	if dashIdx != 1 {
		return "", nil, fmt.Errorf("usage: cc-fleet run <vendor> [-- <claude args>]")
	}
	return args[0], args[1:], nil
}
