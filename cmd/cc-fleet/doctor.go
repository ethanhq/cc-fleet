package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/doctor"
)

// totalChecks is the [N/total] count shown in pretty output. Hard-coded (not
// computed from RunAll's slice) — if you add a tenth check, update this constant
// so the user-facing text stays consistent.
const totalChecks = 9

func newDoctorCmd() *cobra.Command {
	var (
		asJSON bool
		fix    bool
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run the 9 cc-fleet health checks",
		Long: `Run cc-fleet's nine health checks:

  [1/9] ~/.claude/settings.json exists and is valid JSON
  [2/9] ~/.claude/profiles/ writable
  [3/9] tmux installed
  [4/9] claude binary present; version known
  [5/9] at least one attached tmux session (warn — out-of-tmux swarm works without)
  [6/9] all configured vendors' keys reachable (probe /v1/models, 3s/vendor)
  [7/9] skill installed at ~/.claude/skills/cc-fleet/
  [8/9] fingerprint cached and matches current cc version
  [9/9] OAuth credentials.json exists (informational only)

Status semantics: ok = passed; fail = needs action; warn = informational
(doesn't flip overall ok=false).

Exit code: 0 if every check is ok or warn; 1 if any check is fail.

--fix attempts a small set of safe auto-repairs:
  check 2: mkdir -p ~/.claude/profiles (mode 0700)

Other Fixable failures (skill missing, fingerprint stale) print fix hints
but are NOT auto-repaired — they require Claude-orchestrated probes or
manual install.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDoctor(fix, asJSON)
		},
	}

	cmd.Flags().BoolVar(&fix, "fix", false,
		"Attempt safe auto-repairs (currently: mkdir ~/.claude/profiles)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

func runDoctor(fix, asJSON bool) error {
	res := doctor.RunAll(fix)

	if asJSON {
		// Marshal the whole DoctorResult — fields are tagged appropriately
		// in the doctor package. We use Marshal (not MarshalIndent) for the
		// same one-line shape the other cc-fleet --json commands use.
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doctor: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		for _, r := range res.Results {
			fmt.Println(doctor.FormatLine(totalChecks, r))
		}
		if res.OK {
			fmt.Println("\nall checks passed")
		} else {
			fmt.Println("\none or more checks failed; see hints above")
		}
	}

	if !res.OK {
		// cobra suppresses our error printing (SilenceErrors) so this only
		// drives the exit code through main().
		return fmt.Errorf("doctor: one or more checks failed")
	}
	return nil
}
