package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// workflowEnvelope is the --json shape for the workflow command group. It is the
// CLI's own envelope (one per invocation), deliberately separate from
// subagent.Result so a workflow shape change never bloats that contract.
type workflowEnvelope struct {
	OK        bool                   `json:"ok"`
	RunID     string                 `json:"run_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Phases    []subagent.RunPhase    `json:"phases,omitempty"`
	Status    string                 `json:"status,omitempty"`
	StartedAt string                 `json:"started_at,omitempty"`
	Runs      []subagent.WorkflowRun `json:"runs,omitempty"`
	Jobs      []subagent.Result      `json:"jobs,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

// newWorkflowCmd builds `cc-fleet workflow` — run orchestration over subagent
// jobs: declare a run with an ordered phase plan, list runs, and inspect a run's
// jobs. A run manifest is the canonical phase sequencer; member subagents are
// tagged with its run id (`subagent --run-id`).
func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Orchestrate multi-phase subagent runs",
		Long: `Orchestrate a multi-phase workflow run over subagent jobs. Declare a run with
an ordered phase plan, then tag each subagent with the run id (subagent --run-id)
so they group into one run tree on the board. List runs and inspect a run's jobs.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.AddCommand(newWorkflowNewCmd(), newWorkflowListCmd(), newWorkflowStatusCmd())
	return cmd
}

// newWorkflowNewCmd builds `cc-fleet workflow new <name>`. Non-json it prints only
// the bare run id on its own line, so a skill can capture RUN=$(cc-fleet workflow
// new "x"). --json emits one envelope.
func newWorkflowNewCmd() *cobra.Command {
	var (
		phaseTitles []string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:           "new <name>",
		Short:         "Create a workflow run with an ordered phase plan",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Reject duplicate phase titles: the board groups phases by title, so a
			// repeated title would render as one row while the manifest and
			// `workflow status` still list both — a silent divergence.
			seen := map[string]bool{}
			phases := make([]subagent.RunPhase, 0, len(phaseTitles))
			for _, t := range phaseTitles {
				if seen[t] {
					return reportWorkflowErr(fmt.Errorf("duplicate --phase title %q", t), asJSON)
				}
				seen[t] = true
				phases = append(phases, subagent.RunPhase{Title: t})
			}
			run, err := subagent.NewRun(args[0], phases)
			if err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				return emitWorkflow(workflowEnvelope{OK: true, RunID: run.RunID, Name: run.Name,
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt})
			}
			fmt.Println(run.RunID)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&phaseTitles, "phase", nil,
		"Phase title (repeatable; order is the run's phase sequence)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// newWorkflowListCmd builds `cc-fleet workflow list`.
func newWorkflowListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "List workflow runs (newest first)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			runs, err := subagent.ListRuns()
			if err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				return emitWorkflow(workflowEnvelope{OK: true, Runs: runs})
			}
			for _, r := range runs {
				fmt.Printf("%s  %s  %s  %s\n", r.RunID, r.Name, r.Status, r.StartedAt)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// newWorkflowStatusCmd builds `cc-fleet workflow status <run-id>`. Run-id
// validation happens inside subagent.ReadRun (it becomes a path component).
func newWorkflowStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "status <run-id>",
		Short:         "Show a workflow run and its subagent jobs",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			run, jobs, err := subagent.RunStatus(args[0])
			if err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				return emitWorkflow(workflowEnvelope{
					OK: true, RunID: run.RunID, Name: run.Name,
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt, Jobs: jobs,
				})
			}
			fmt.Printf("run %s  %s  %s\n", run.RunID, run.Name, run.Status)
			for _, j := range jobs {
				fmt.Printf("  %s  %s  %s  %s\n", j.Phase, j.Label, j.Status, j.JobID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// emitWorkflow marshals one envelope to stdout and returns nil (cobra then exits
// 0); a marshal failure exits 1. Mirrors the subagent reporter's single-envelope
// contract.
func emitWorkflow(env workflowEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workflow: marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
	return nil
}

// reportWorkflowErr renders a workflow error: --json emits {"ok":false,"error":..}
// + exit 1; non-json writes a stderr line + exit 1.
func reportWorkflowErr(err error, asJSON bool) error {
	if asJSON {
		data, merr := json.Marshal(workflowEnvelope{OK: false, Error: err.Error()})
		if merr != nil {
			fmt.Fprintln(os.Stderr, "workflow: marshal:", merr)
			os.Exit(1)
		}
		fmt.Println(string(data))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "workflow:", err)
	os.Exit(1)
	return nil
}
