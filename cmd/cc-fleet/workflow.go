package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/workflow"
)

// workflowEnvelope is the --json shape for the workflow command group. It is the
// CLI's own envelope (one per invocation), deliberately separate from
// subagent.Result so a workflow shape change never bloats that contract.
type workflowEnvelope struct {
	OK        bool                     `json:"ok"`
	RunID     string                   `json:"run_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Phases    []subagent.RunPhase      `json:"phases,omitempty"`
	Status    string                   `json:"status,omitempty"`
	StartedAt string                   `json:"started_at,omitempty"`
	Runs      []subagent.WorkflowRun   `json:"runs,omitempty"`
	Jobs      []subagent.Result        `json:"jobs,omitempty"`
	Saved     []subagent.SavedWorkflow `json:"saved,omitempty"`
	RunError  string                   `json:"run_error,omitempty"` // a failed run's cause (distinct from the command-level Error)
	Error     string                   `json:"error,omitempty"`
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
	cmd.AddCommand(newWorkflowNewCmd(), newWorkflowListCmd(), newWorkflowStatusCmd(), newWorkflowRunCmd(), newWorkflowStopCmd(), newWorkflowWatchCmd(), newWorkflowSavedCmd())
	return cmd
}

// newWorkflowWatchCmd builds `cc-fleet workflow watch <run-id>` — stream a run's live events as
// scrubbed text until it finishes, so a detached run is observable from a plain terminal (or a
// backgrounded shell → the /tasks panel, or the `cc-fleet:workflow-watch` agent → FleetView).
// Exit code: done/stopped→0, failed→1, timed-out-while-running→124 (reattach with --since-seq),
// SIGINT→130, watcher/IO/unknown-run error→2 (distinct from a run's own failure).
func newWorkflowWatchCmd() *cobra.Command {
	var (
		timeout  time.Duration
		interval time.Duration
		sinceSeq int64
	)
	cmd := &cobra.Command{
		Use:   "watch <run-id>",
		Short: "Stream a workflow run's live status until it finishes",
		Long: `Stream a workflow run's live events as text until the run reaches a terminal status.
Blocks (no busy-poll); prints one scrubbed line per event and a final status line. Run it in a
backgrounded shell to surface the run in the /tasks panel, or via the cc-fleet:workflow-watch
agent to surface it in the agent panel.`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			run, werr := workflow.Watch(ctx, args[0], os.Stdout,
				workflow.WatchOptions{Interval: interval, SinceSeq: sinceSeq})
			switch {
			case werr == nil:
				if run.Status == "failed" {
					os.Exit(1)
				}
				return nil // done / stopped
			case errors.Is(werr, workflow.ErrEngineGone):
				os.Exit(1)
			case errors.Is(werr, context.DeadlineExceeded):
				os.Exit(124) // timed out while still running — reattach (see --since-seq)
			case errors.Is(werr, context.Canceled):
				os.Exit(130) // interrupted
			default:
				fmt.Fprintln(os.Stderr, "workflow:", werr)
				os.Exit(2) // watcher / IO / unknown-run error
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0,
		"Stop watching after this duration (0 = until the run finishes / interrupted)")
	cmd.Flags().DurationVar(&interval, "interval", 0, "Poll cadence (default ~500ms)")
	cmd.Flags().Int64Var(&sinceSeq, "since-seq", 0,
		"Skip events with seq <= N for a clean reattach (scoped to one run generation; a resume restarts seq)")
	return cmd
}

// newWorkflowRunCmd builds `cc-fleet workflow run <script.star>` — execute a Starlark
// orchestration script. By default it mints the run, re-execs cc-fleet as a detached
// child that runs the engine off the launching process, and prints the bare run id
// (so the main session is never blocked for the run's duration). --foreground runs
// inline to completion (debugging + the deterministic e2e). The hidden --run-id names
// an already-minted manifest and is set only by the detached re-exec.
func newWorkflowRunCmd() *cobra.Command {
	var (
		foreground     bool
		runID          string
		resume         string
		maxConcurrency int
		argsJSON       string
		noPersistIO    bool
		budgetUSD      float64
		leadSessionID  string
		saved          string
		asJSON         bool
	)
	cmd := &cobra.Command{
		Use:   "run <script.star>",
		Short: "Run a Starlark workflow script (orchestrates vendor subagents off the main context)",
		Long: `Run a Starlark workflow script that fans out vendor subagents. The script's
plan executes in a cc-fleet process, NOT the main Claude context: it declares a meta
dict and calls agent()/parallel()/pipeline()/phase()/log(). By default the run is
launched detached and this prints the bare run id; poll it with 'workflow status' or
watch the board. --foreground runs inline to completion instead.`,
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Resolve the script: --saved <name> re-runs a saved workflow, else the positional path (one
			// is required — cobra allows 0 args so --saved can stand alone; the default case enforces it).
			var script string
			switch {
			case saved != "":
				p, serr := subagent.SavedWorkflowScript(saved)
				if serr != nil {
					return reportWorkflowErr(serr, asJSON)
				}
				script = p
			case len(args) == 1:
				script = args[0]
			default:
				return reportWorkflowErr(fmt.Errorf("workflow run needs a script path or --saved <name>"), asJSON)
			}
			opts := workflow.Options{RunID: runID, Resume: resume, Concurrency: maxConcurrency, ArgsJSON: argsJSON, NoPersistIO: noPersistIO, BudgetUSD: budgetUSD}
			// Capture the launching Claude session on a genuine FRESH launch only (the detached
			// --run-id re-exec reads it back off the manifest; a --resume preserves it), so the
			// board groups runs by session like the teammates board. An explicit flag wins.
			if runID == "" && resume == "" {
				if opts.LeadSessionID = leadSessionID; opts.LeadSessionID == "" {
					opts.LeadSessionID = leadsession.Detect()
				}
			}
			// SIGINT (Ctrl-C on a --foreground run) and SIGTERM (a kill of the detached
			// child, e.g. by teardown) cancel the run: queued leaves stop launching and
			// the manifest is finalized off "running" instead of being stranded. An
			// in-flight leaf still drains to its own per-agent timeout.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The detached/foreground re-exec carries --run-id: its manifest is already
			// minted, so it executes the body directly instead of re-preparing.
			if runID != "" {
				if err := workflow.Execute(ctx, script, runID, opts); err != nil {
					return reportWorkflowErr(err, asJSON)
				}
				return nil
			}

			// A detached --resume serializes against the board's restart and any concurrent
			// resume/stop via the per-run execution lock; a foreground resume IS the engine
			// running inline (it self-stamps its pid synchronously) and must not hold the lock
			// around Execute, so it is not wrapped.
			id, err := func() (string, error) {
				if resume != "" && !foreground {
					var rid string
					lerr := subagent.WithRunLock(resume, func() error {
						var e error
						rid, e = workflow.Launch(ctx, script, opts, foreground)
						return e
					})
					return rid, lerr
				}
				return workflow.Launch(ctx, script, opts, foreground)
			}()
			if err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				run, _, rerr := subagent.RunStatus(id)
				if rerr != nil {
					return emitWorkflow(workflowEnvelope{OK: true, RunID: id, Status: "running"})
				}
				return emitWorkflow(workflowEnvelope{OK: true, RunID: run.RunID, Name: run.Name,
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt, RunError: run.Error})
			}
			fmt.Println(id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false,
		"Run inline to completion instead of detaching")
	cmd.Flags().StringVar(&runID, "run-id", "", "Execute an already-minted run (internal)")
	_ = cmd.Flags().MarkHidden("run-id")
	cmd.Flags().StringVar(&resume, "resume", "",
		"Resume an existing run id: replay its journaled leaves (no re-exec) and run only the rest")
	cmd.Flags().IntVar(&maxConcurrency, "max-concurrency", 0,
		"Max concurrent vendor leaves (default: min(16, cores-2))")
	cmd.Flags().StringVar(&argsJSON, "args-json", "",
		"JSON value passed to the script as `args`")
	cmd.Flags().BoolVar(&noPersistIO, "no-persist-io", false,
		"Don't persist leaf prompts/answers for board drill-in (persistence is default-on)")
	cmd.Flags().Float64Var(&budgetUSD, "budget-usd", 0,
		"Cap total vendor spend in USD; agent() fails once reached (0 = uncapped)")
	cmd.Flags().StringVar(&leadSessionID, "lead-session-id", "",
		"Parent Claude session id for board grouping (default: auto-detect)")
	cmd.Flags().StringVar(&saved, "saved", "",
		"Re-run a saved workflow by name (instead of a script path)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// newWorkflowSavedCmd builds `cc-fleet workflow saved` — list the named workflows saved from the board
// (newest first), so an agent can discover + re-run one with `workflow run --saved <name>`.
func newWorkflowSavedCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "saved",
		Short:         "List saved workflows (newest first)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			saved, err := subagent.ListSavedWorkflows()
			if err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				return emitWorkflow(workflowEnvelope{OK: true, Saved: saved})
			}
			for _, s := range saved {
				fmt.Printf("%s\t%s\t%s\n", s.Name, s.SessionID, s.SavedAt)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
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
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt, RunError: run.Error})
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
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt,
					RunError: run.Error, Jobs: jobs,
				})
			}
			fmt.Printf("run %s  %s  %s\n", run.RunID, run.Name, run.Status)
			if run.Error != "" {
				fmt.Printf("  error: %s\n", run.Error)
			}
			for _, j := range jobs {
				fmt.Printf("  %s  %s  %s  %s\n", j.Phase, j.Label, j.Status, j.JobID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// newWorkflowStopCmd builds `cc-fleet workflow stop <run-id>` — reap a running workflow
// run: kill the engine's process tree (its in-flight vendor leaves included, behind a
// cmdline reuse guard) and mark the manifest stopped. Restart it with `workflow run
// <script> --resume <run-id>` (the journal makes the replay cheap).
func newWorkflowStopCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "stop <run-id>",
		Short:         "Stop a running workflow run (reap its engine + in-flight leaves)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Serialize stop against a concurrent restart/resume of the same run via the per-run
			// execution lock (so a stop can't race the pre-launch pid window into skipping the kill).
			var run subagent.WorkflowRun
			if err := subagent.WithRunLock(args[0], func() error {
				var e error
				run, e = subagent.StopRun(args[0])
				return e
			}); err != nil {
				return reportWorkflowErr(err, asJSON)
			}
			if asJSON {
				return emitWorkflow(workflowEnvelope{OK: true, RunID: run.RunID, Name: run.Name,
					Phases: run.Phases, Status: run.Status, StartedAt: run.StartedAt})
			}
			fmt.Printf("stopped %s  %s\n", run.RunID, run.Name)
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
