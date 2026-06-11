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
		Use:   "run [provider] [-- <claude args>]",
		Short: "Launch an interactive claude session backed by a provider",
		Long: "Replace this process with an interactive `claude` REPL whose LLM backend is the\n" +
			"named provider: its profile pins the apiKeyHelper + base URL, and the model is the\n" +
			"provider's default_model unless --model overrides. The provider key never enters env,\n" +
			"argv, or shell history. Requires an interactive terminal.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			requested, extra, err := splitRunArgs(args, cmd.ArgsLenAtDash())
			if err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet run:", err)
				os.Exit(2)
			}
			provider, err := resolveProviderArg(requested)
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
			if err := run.Run(run.Request{Provider: provider, Model: model, PermissionMode: permMode, ExtraArgs: extra}); err != nil {
				fmt.Fprintln(os.Stderr, "cc-fleet run:", err)
				os.Exit(1)
			}
			return nil // unreachable: a successful run.Run replaced the process
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "Provider model id (default: the provider's default_model)")
	cmd.Flags().StringVar(&permissionMode, "permission-mode", "",
		"Permission mode for the session (default|acceptEdits|plan|auto|bypassPermissions)")
	cmd.Flags().BoolVar(&dangerSkip, "dangerously-skip-permissions", false,
		"Skip permission prompts (alias for --permission-mode bypassPermissions)")
	return cmd
}

// splitRunArgs separates an OPTIONAL provider positional from the post-"--" claude
// passthrough. dashIdx is cobra Command.ArgsLenAtDash() (-1 when no "--" was
// given). A blank provider ("") means "use the default provider" — the caller
// resolves it. Pure, so the arg contract is unit-tested without running the
// command. Shapes: `run` / `run <v>` / `run -- <args>` / `run <v> -- <args>`.
func splitRunArgs(args []string, dashIdx int) (provider string, extra []string, err error) {
	if dashIdx < 0 {
		switch len(args) {
		case 0:
			return "", nil, nil
		case 1:
			return args[0], nil, nil
		default:
			return "", nil, fmt.Errorf("usage: cc-fleet run [<provider>] [-- <claude args>]")
		}
	}
	// dashIdx counts the positionals BEFORE "--": 0 = default provider, 1 = explicit.
	switch dashIdx {
	case 0:
		return "", args, nil
	case 1:
		return args[0], args[1:], nil
	default:
		return "", nil, fmt.Errorf("usage: cc-fleet run [<provider>] [-- <claude args>]")
	}
}
