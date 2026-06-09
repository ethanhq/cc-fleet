package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/selfupdate"
)

func newUpdateCmd() *cobra.Command {
	var (
		checkOnly  bool
		binaryOnly bool
		force      bool
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the cc-fleet binary and Claude Code plugin to the latest release",
		Long: `Update cc-fleet to the latest GitHub release.

The binary is updated according to how it was installed:
  - curl|sh / release tarball: downloaded, sha256-verified, smoke-tested, then
    atomically swapped in place (the previous binary is kept as <bin>.previous).
  - npm:        runs 'npm install -g @ethanhq/cc-fleet@latest'.
  - go install: runs 'go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest'.
When the needed tool is missing (or the install dir isn't writable), update
prints the exact command instead of acting.

The Claude Code plugin is refreshed in the same run (marketplace update + plugin
update, preserving scope); --binary-only skips it. A dev/non-release build is
reported as not comparable and is never updated.

  cc-fleet update            update the binary + plugin
  cc-fleet update --check    report current vs latest, change nothing
  cc-fleet update rollback   restore the previous binary`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if checkOnly {
				return runUpdateCheck(ctx)
			}
			return selfupdate.Run(ctx, selfupdate.Options{
				BinaryOnly: binaryOnly,
				Force:      force,
				Out:        os.Stdout,
			})
		},
	}

	cmd.Flags().BoolVar(&checkOnly, "check", false,
		"Report current vs latest without updating")
	cmd.Flags().BoolVar(&binaryOnly, "binary-only", false,
		"Update only the binary, not the Claude Code plugin")
	cmd.Flags().BoolVar(&force, "force", false,
		"Self-update in place even when the install method can't be determined")

	cmd.AddCommand(&cobra.Command{
		Use:           "rollback",
		Short:         "Restore the previous binary kept by the last update",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return selfupdate.Rollback(os.Stdout)
		},
	})

	return cmd
}

// runUpdateCheck prints the current-vs-latest state and always exits 0 — it is
// informational; an offline check is a notice, not a failure.
func runUpdateCheck(ctx context.Context) error {
	st, err := selfupdate.Check(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cc-fleet %s — could not check for updates: %s\n", st.Current, err)
		return nil
	}
	switch {
	case !st.Comparable:
		fmt.Printf("Development build (%s) — not comparable. Latest release is %s.\n", st.Current, st.Latest)
	case st.NewerAvailable:
		fmt.Printf("cc-fleet %s  ·  latest %s  → run `ccf update`\n", st.Current, st.Latest)
	default:
		fmt.Printf("cc-fleet %s is the latest.\n", st.Current)
	}
	return nil
}
