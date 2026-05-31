package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/secrets"
)

// newKeygetCmd builds the `cc-fleet keyget <vendor>` command. It's wired up so
// that the API key is written to stdout exactly once with no trailing newline,
// and any error path writes to stderr only — matching Claude Code's
// apiKeyHelper contract (stdout = key, exit code 0 = success).
func newKeygetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "keyget <vendor>",
		Short: "Fetch vendor API key (used by Claude Code apiKeyHelper)",
		Long: `Resolve <vendor>'s API key via its configured secret_backend and
write it to stdout. The key never appears in logs or in this process's
environment. Invoked automatically by Claude Code through apiKeyHelper.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := secrets.Keyget(args[0])
			if err != nil {
				// Print our own error format to stderr and exit non-zero
				// without letting cobra echo the error a second time.
				fmt.Fprintln(os.Stderr, "ERROR:", err)
				os.Exit(1)
			}
			// stdout = key only. secrets.Keyget already trimmed trailing
			// CR/LF; we deliberately do not append another newline.
			if _, err := os.Stdout.Write(key); err != nil {
				fmt.Fprintln(os.Stderr, "ERROR: write stdout:", err)
				os.Exit(1)
			}
			return nil
		},
	}
}
