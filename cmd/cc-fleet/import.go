package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// importJSONEnvelope is the JSON shape `cc-fleet import --json` emits. RepairFailed is
// set only if the post-import profile rebuild failed (the roster is still committed —
// re-run `cc-fleet repair`).
type importJSONEnvelope struct {
	OK               bool     `json:"ok"`
	Added            []string `json:"added"`
	Overwritten      []string `json:"overwritten"`
	SkippedCollision []string `json:"skipped_collision"`
	SkippedCodex     []string `json:"skipped_codex"`
	DefaultSet       string   `json:"default_set,omitempty"`
	DefaultDropped   string   `json:"default_dropped,omitempty"`
	BackupPath       string   `json:"backup_path,omitempty"`
	RepairFailed     string   `json:"repair_failed,omitempty"`
}

func newImportCmd() *cobra.Command {
	var force, asJSON bool

	cmd := &cobra.Command{
		Use:   "import <bundle.toml>",
		Short: "Apply a keyless provider bundle from another machine",
		Long: `Apply a provider bundle written by ` + "`cc-fleet export`" + `. The whole bundle is
validated before anything is written, so a bad bundle leaves the roster untouched. A
provider that collides with an existing one is skipped unless --force; --force backs up
providers.toml before replacing it in a single atomic write. codex providers in the
bundle are skipped.

Import writes config only and never contacts a provider; profiles are rebuilt afterward.
Only import a bundle you trust — its base_url / models_endpoint / secret references drive
later local secret reads and key-bearing requests.

After import, re-enter keys for file-backend providers via
` + "`cc-fleet edit --api-key-stdin/--api-key-file`" + `, re-run ` + "`cc-fleet codex login`" + ` for any
codex providers, then ` + "`cc-fleet doctor`" + `.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			res, err := userops.Import(userops.ImportRequest{Bundle: data, Force: force})
			if err != nil {
				reportUserOpErr(asJSON, err)
				return err
			}
			// providers.toml is committed; rebuild profiles separately (a derived cache).
			repairErr := ""
			if _, e := userops.Repair(); e != nil {
				repairErr = e.Error()
			}

			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(importJSONEnvelope{
					OK: true, Added: res.Added, Overwritten: res.Overwritten,
					SkippedCollision: res.SkippedCollision, SkippedCodex: res.SkippedCodex,
					DefaultSet: res.DefaultSet, DefaultDropped: res.DefaultDropped,
					BackupPath: res.BackupPath, RepairFailed: repairErr,
				})
			}

			fmt.Printf("imported: %d added, %d overwritten, %d skipped (collision), %d skipped (codex)\n",
				len(res.Added), len(res.Overwritten), len(res.SkippedCollision), len(res.SkippedCodex))
			if res.BackupPath != "" {
				fmt.Printf("backed up previous config to %s\n", res.BackupPath)
			}
			if res.DefaultSet != "" {
				fmt.Printf("default provider: %s\n", res.DefaultSet)
			}
			if res.DefaultDropped != "" {
				fmt.Printf("default %q not applied (target absent/reserved, or would change the existing default without --force)\n", res.DefaultDropped)
			}
			if len(res.SkippedCodex) > 0 {
				fmt.Printf("codex skipped — run `cc-fleet codex add` + `cc-fleet codex login` here: %v\n", res.SkippedCodex)
			}
			fmt.Println("re-enter keys for file-backend providers (`cc-fleet edit --api-key-stdin/--api-key-file`), then `cc-fleet doctor`.")
			if repairErr != "" {
				fmt.Fprintf(os.Stderr, "warning: profile rebuild failed (re-run `cc-fleet repair`): %s\n", repairErr)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite colliding providers (backs up providers.toml first)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON result envelope")
	return cmd
}
