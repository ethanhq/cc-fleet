package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// uninstallJSONEnvelope is the JSON shape `cc-fleet uninstall --json` emits.
// Removed enumerates the paths that were deleted; Kept enumerates the paths
// we deliberately left alone (or failed to remove and surfaced as a soft
// note rather than an error); Manual enumerates the commands the user must
// run to finish an --all uninstall.
type uninstallJSONEnvelope struct {
	OK      bool     `json:"ok"`
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
	Manual  []string `json:"manual,omitempty"`
}

func newUninstallCmd() *cobra.Command {
	var (
		keepSecrets bool
		wipeSecrets bool
		all         bool
		yes         bool
		asJSON      bool
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove cc-fleet state — or everything, with --all",
		Long: `Remove every state file cc-fleet manages on disk:

  ~/.claude/profiles/<provider>.json   (one per provider)
  ~/.config/cc-fleet/providers.toml
  ~/.config/cc-fleet/fingerprint.json
  ~/.config/cc-fleet/models-cache.json
  ~/.config/cc-fleet/onboarding.json
  ~/.config/cc-fleet/update-check.json
  ~/.config/cc-fleet/subagent-jobs/  (finished background subagent jobs)

Per-provider file-backend secrets in ~/.config/cc-fleet/secrets/ are
preserved by default (--keep-secrets, the default). Pass --wipe-secrets to
remove the entire secrets/ directory.

Background subagent jobs that are still running are left intact (with a
note on stderr) so uninstall never yanks files from a live job; reap them
later with ` + "`cc-fleet subagent-gc`" + ` once they finish (or just re-run uninstall).

A bare uninstall is a re-installable state reset — it does NOT touch:
  ~/.claude/skills/                   (owned by install machinery)
  ~/.claude/teams/                    (owned by Claude Code itself)
  the cc-fleet binary, ccf alias, and Claude Code plugin

--all removes those too: the cc-fleet skill dirs, the plugin (via the claude
CLI when available), and — last — the binary, ccf alias, install manifest,
and rollback backup, routed by install method (npm uninstall -g for an npm
install; direct file removal for tarball / go install; an unknown method is
never guess-deleted — the exact commands are printed instead, as is
everything on Windows, where a running exe can't be deleted). Under --all
secrets/ is wiped too unless --keep-secrets is passed explicitly. --all asks
for confirmation; non-interactive and --json callers must pass --yes.

Uninstall is idempotent — missing files are not an error. After a bare
uninstall you can run ` + "`cc-fleet init`" + ` again to start over.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Both flags are only a real conflict when the user explicitly
			// set --keep-secrets=true AND --wipe-secrets — bare --wipe-secrets
			// on its own is fine even though --keep-secrets defaults to true.
			if cmd.Flags().Changed("keep-secrets") && wipeSecrets && keepSecrets {
				err := fmt.Errorf("--keep-secrets and --wipe-secrets are mutually exclusive")
				reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeUninstallFailed, Err: err})
				return err
			}
			// keep wins unless wipe is set; explicit --keep-secrets=false also
			// wipes. --all flips the default: secrets go unless explicitly kept.
			keep := keepSecrets && !wipeSecrets
			if all {
				keep = cmd.Flags().Changed("keep-secrets") && keepSecrets && !wipeSecrets
			}

			if all && !yes {
				if uninstallAllNeedsYes(asJSON, os.Stdin) {
					err := fmt.Errorf("--all is destructive; use with --json or non-interactively requires --yes")
					reportUserOpErr(asJSON, &userops.Op{Code: userops.CodeUninstallFailed, Err: err})
					return err
				}
				secretsNote := "WIPED"
				if keep {
					secretsNote = "kept"
				}
				ans, _ := promptLine(bufio.NewReader(os.Stdin), fmt.Sprintf(
					"Remove cc-fleet COMPLETELY — binary, skills/plugin, all state (secrets %s)? (y/N): ", secretsNote))
				ans = strings.TrimSpace(strings.ToLower(ans))
				if ans != "y" && ans != "yes" {
					fmt.Println("aborted — nothing removed")
					return nil
				}
			}

			res, err := userops.Uninstall(userops.UninstallRequest{KeepSecrets: keep, All: all})
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			if asJSON {
				emitJSON(uninstallJSONEnvelope{
					OK:      true,
					Removed: res.Removed,
					Kept:    res.Kept,
					Manual:  res.Manual,
				})
				return nil
			}
			fmt.Printf("uninstalled cc-fleet (removed %d path(s), kept %d)\n",
				len(res.Removed), len(res.Kept))
			for _, p := range res.Removed {
				fmt.Println("  removed:", p)
			}
			for _, p := range res.Kept {
				fmt.Println("  kept:   ", p)
			}
			if !all {
				fmt.Println("note: the binary, ccf alias, skills, and plugin stay — `cc-fleet uninstall --all` removes those too")
			}
			if len(res.Manual) > 0 {
				fmt.Println("finish manually:")
				for _, c := range res.Manual {
					fmt.Println("  " + c)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&keepSecrets, "keep-secrets", true,
		"Preserve ~/.config/cc-fleet/secrets/ (the default without --all)")
	cmd.Flags().BoolVar(&wipeSecrets, "wipe-secrets", false,
		"Remove ~/.config/cc-fleet/secrets/ entirely (overrides --keep-secrets)")
	cmd.Flags().BoolVar(&all, "all", false,
		"Also remove the skills, plugin, binary, and (unless --keep-secrets) secrets")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"Skip the --all confirmation prompt (required when non-interactive)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

// uninstallAllNeedsYes reports whether `--all` without --yes must refuse
// instead of prompting: under --json a prompt (or a no-envelope abort) would
// corrupt the single-envelope stdout contract, and a non-interactive stdin
// can't answer a prompt at all. This destructive gate uses a real terminal
// check — isTTY's char-device heuristic reads /dev/null as interactive, which
// would let automation "abort" with exit 0 and believe the uninstall ran.
func uninstallAllNeedsYes(asJSON bool, stdin *os.File) bool {
	return asJSON || !term.IsTerminal(stdin.Fd())
}
