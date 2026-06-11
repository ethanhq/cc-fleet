package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/doctor"
)

// totalChecks is the [N/total] count shown in pretty output. Hard-coded (not
// computed from RunAll's slice) — keep it in step with RunAll's check list.
const totalChecks = 10

func newDoctorCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run the cc-fleet health checks",
		Long: `Run cc-fleet's health checks, grouped Core vs Optional.

Core (every run mode — subagent / workflow / run / teammate):
  [1] ~/.claude/settings.json exists and is valid JSON
  [2] ~/.claude/profiles/ writable
  [4] claude binary present; version known
  [6] all configured providers' keys reachable (probe /v1/models, 3s/provider)
  [7] skill installed at ~/.claude/skills/cc-fleet/ (or via plugin)
  [8] fingerprint cached and matches current cc version
  [9] OAuth credentials.json exists (informational only)
  [10] binary and plugin versions match

Optional — live teammates only (tmux):
  [3] tmux installed (warn — subagent / workflow / run work without it)
  [5] at least one attached tmux session (warn — out-of-tmux swarm works without)

Status semantics: ok = passed; fail = needs action; warn = informational.

Exit code: 0 when every Core check is ok/warn; 1 only when a Core check fails.
An Optional (tmux) warning never fails doctor.

Doctor never repairs anything — failures print fix hints for the user (or the
skill) to act on.`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDoctor(asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit a machine-readable JSON envelope (for skill consumption)")

	return cmd
}

func runDoctor(asJSON bool) error {
	res := doctor.RunAll()

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
		printDoctorGroup("Core", doctor.GroupCore, res.Results)
		printDoctorGroup("Optional — live teammates only", doctor.GroupOptional, res.Results)
		if res.OK {
			fmt.Println("core checks passed")
		} else {
			fmt.Println("one or more core checks failed; see hints above")
		}
	}

	if !res.OK {
		// cobra suppresses our error printing (SilenceErrors) so this only
		// drives the exit code through main().
		return fmt.Errorf("doctor: one or more core checks failed")
	}
	return nil
}

// printDoctorGroup prints one section header and its check lines, or nothing if
// the group has no results. Lines are indented two spaces under the header.
func printDoctorGroup(title string, g doctor.Group, results []doctor.CheckResult) {
	var lines []string
	for _, r := range results {
		if r.Group == g {
			lines = append(lines, doctor.FormatLine(totalChecks, r))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Println(title)
	for _, l := range lines {
		fmt.Println("  " + l)
	}
	fmt.Println()
}
