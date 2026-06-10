package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// maxRunTagLen caps --phase / --label (opaque display metadata) so a job file
// can't be bloated by an oversized tag. --run-id is validated separately (it
// becomes a filesystem path component for the run manifest).
const maxRunTagLen = 256

// newSubagentCmd builds `cc-fleet subagent <vendor>` — a one-shot headless
// vendor subagent. It follows the spawn command's --json / SilenceErrors
// discipline so the skill gets exactly one envelope on stdout.
func newSubagentCmd() *cobra.Command {
	var (
		prompt         string
		promptFile     string
		model          string
		outputFormat   string
		permissionMode string
		resume         string
		timeout        time.Duration
		probe          bool
		asJSON         bool
		background     bool
		maxTurns       int
		maxBudget      float64
		leadSessionID  string
		runID          string
		phase          string
		label          string
		promptProfile  string
		tools          string
		skills         bool
		mcp            bool
		noPersistIO    bool
	)

	cmd := &cobra.Command{
		Use:   "subagent <vendor>",
		Short: "Run a one-shot headless vendor subagent (Claude layer)",
		Long: `Run a one-shot, headless vendor subagent: launch claude -p backed by a
third-party vendor model (via the vendor profile) and return the result
synchronously. The analog of the native Agent/Task tool, but the model can be
a vendor id. No tmux pane, no team, no locks.

Designed to be invoked by the cc-fleet skill via Bash with --json, which
emits one machine-readable subagent.Result envelope the skill switches on.

slim is the DEFAULT: a native-mirror agent prompt + a restricted tool whitelist (Bash,
Edit, Glob, Grep, Read, Skill, Write; slim-ro is read-only: Bash, Glob, Grep, Read,
Skill). Any tool beyond the whitelist (e.g. WebSearch) must be passed explicitly via
--tools — a whole-set replacement, not an append. --profile full restores the full
session prompt (for behavior comparison). --skills / --mcp refine a slim run; slim
inherits the host MCP config by default, slim-ro stays strict.

For long-running or research tasks (web search, many turns — anything that may
exceed the sync timeout), prefer --background: the job runs detached, returns a
job handle immediately, and you poll subagent-status (a wedged sync subagent can't
be polled). Hitting --max-budget-usd returns a SUBAGENT_FAILED envelope whose
suggestion names the spent cost and how to retry (raise the cap or switch model).`,
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			vendor := args[0]

			// Validate exactly-one-of(--prompt, --prompt-file) at the CLI layer
			// (no claude launch on bad args).
			hasPrompt := cmd.Flags().Changed("prompt")
			hasFile := cmd.Flags().Changed("prompt-file")
			if hasPrompt == hasFile {
				res := subagent.Result{
					OK:        false,
					ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg:  "exactly one of --prompt or --prompt-file is required",
					Vendor:    vendor,
				}
				return reportSubagent(res, asJSON)
			}

			// Workflow tags: --run-id becomes a run-manifest path component, so it
			// gets the full path-safe id validation; --phase / --label are opaque
			// display metadata, length-capped only — their control-byte / format
			// sanitization is the board's job at render time, not here (they are
			// stored injection-safe via encoding/json).
			if runID != "" {
				if err := ids.ValidateJobID(runID); err != nil {
					res := subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
						ErrorMsg: fmt.Sprintf("invalid --run-id: %v", err), Vendor: vendor}
					return reportSubagent(res, asJSON)
				}
			}
			if len(phase) > maxRunTagLen || len(label) > maxRunTagLen {
				res := subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg: fmt.Sprintf("--phase and --label must each be at most %d bytes", maxRunTagLen),
					Vendor:   vendor}
				return reportSubagent(res, asJSON)
			}

			// Front-load the prompt-profile + slim-refinement validation as
			// SUBAGENT_BAD_ARGS before any side effect: the profile enum, the
			// slim-only refinements rejected when combined with full, and the
			// --tools parse + canonicalization.
			if err := subagent.ValidateProfile(promptProfile); err != nil {
				return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg: err.Error(), Vendor: vendor}, asJSON)
			}
			isFull := promptProfile == "" || promptProfile == subagent.ProfileFull
			toolList, err := splitToolsCSV(tools)
			if err != nil {
				return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg: fmt.Sprintf("invalid --tools: %v", err), Vendor: vendor}, asJSON)
			}
			noSkills := !skills
			if isFull && (len(toolList) > 0 || noSkills || cmd.Flags().Changed("mcp")) {
				return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg: "--tools / --skills=false / --mcp are slim-only; pass --profile slim or slim-ro",
					Vendor:   vendor}, asJSON)
			}
			if len(toolList) > 0 {
				if _, err := subagent.CanonicalizeTools(toolList); err != nil {
					return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
						ErrorMsg: fmt.Sprintf("invalid --tools: %v", err), Vendor: vendor}, asJSON)
				}
			}
			if err := subagent.ValidateToolsSkills(toolList, noSkills); err != nil {
				return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeBadArgs,
					ErrorMsg: err.Error(), Vendor: vendor}, asJSON)
			}
			// MCP per-profile default when --mcp wasn't given: slim inherits the host
			// config (native generic parity), slim-ro stays strict; for full the value is
			// inert. An explicit --mcp (either value) wins.
			mcp = resolveMCPDefault(cmd.Flags().Changed("mcp"), mcp, promptProfile)
			// An explicitly chosen non-full profile across a resumed session silently
			// swaps the agent's system prompt mid-conversation; warn but don't fail
			// (profile consistency across turns is the caller's responsibility). The
			// default-slim resume stays silent.
			if resume != "" && cmd.Flags().Changed("profile") && !isFull {
				fmt.Fprintf(os.Stderr, "subagent: warning: --resume with --profile %s swaps the system prompt mid-session; keep the profile constant across turns\n", promptProfile)
			}

			req := subagent.Request{
				Vendor:         vendor,
				Model:          model,
				Prompt:         prompt,
				OutputFormat:   outputFormat,
				JSON:           asJSON,
				Timeout:        timeout,
				Probe:          probe,
				PermissionMode: permissionMode,
				Resume:         resume,
				Background:     background,
				MaxTurns:       maxTurns,
				MaxBudgetUSD:   maxBudget,
				LeadSessionID:  leadSessionID,
				RunID:          runID,
				Phase:          phase,
				Label:          label,
				PromptProfile:  promptProfile,
				Tools:          toolList,
				NoSkills:       noSkills,
				MCP:            mcp,
				PersistIO:      !noPersistIO,
			}
			if req.PersistIO {
				// The .prompt sidecar needs the text in hand; a --prompt-file /
				// stdin prompt streams to claude and is not captured.
				req.IOPrompt = prompt
				// Live tool/usage activity flips claude to stream-json, which would
				// replace the raw output every non---json path passes through verbatim
				// (plain text AND --output-format json) — stream only on the --json
				// skill path, whose envelope is distilled, not passed through (and
				// never for a detached background job, mirroring workflow leaves).
				req.StreamActivity = !background && asJSON
			}

			if hasFile {
				f, shouldClose, err := openPromptFile(promptFile)
				if err != nil {
					res := subagent.Result{
						OK:        false,
						ErrorCode: subagent.ErrCodeBadArgs,
						ErrorMsg:  err.Error(),
						Vendor:    vendor,
					}
					return reportSubagent(res, asJSON)
				}
				req.PromptReader = f
				if shouldClose {
					// Closes after Run returns (sync) or after Start+Release
					// (background), where the child already holds its own fd.
					defer f.Close()
				}
			}

			res := subagent.Run(req)
			return reportSubagent(res, asJSON)
		},
	}

	cmd.Flags().StringVar(&prompt, "prompt", "",
		"Task prompt (mutually exclusive with --prompt-file)")
	cmd.Flags().StringVar(&promptFile, "prompt-file", "",
		"Read the prompt from a file (or '-' for stdin); keeps large/sensitive prompts out of argv")
	cmd.Flags().StringVar(&model, "model", "",
		"Vendor model id (default: vendor's default_model)")
	cmd.Flags().StringVar(&outputFormat, "output-format", "text",
		"claude inner output format: text|json (passthrough; ignored when --json forces json)")
	cmd.Flags().DurationVar(&timeout, "timeout", 300*time.Second,
		"Hard wall-clock timeout; on expiry the whole process group is killed")
	cmd.Flags().BoolVar(&probe, "probe", false,
		"Probe vendor reachability before running (3s; default off, opposite of spawn)")
	cmd.Flags().StringVar(&permissionMode, "permission-mode", "",
		"claude permission mode (default: --dangerously-skip-permissions)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit cc-fleet's machine-readable Result envelope (skill path; forces inner json)")
	cmd.Flags().IntVar(&maxTurns, "max-turns", 0,
		"Cap agentic turns (passed to claude --max-turns)")
	cmd.Flags().Float64Var(&maxBudget, "max-budget-usd", 0,
		"Cap spend in USD (passed to claude --max-budget-usd)")
	cmd.Flags().BoolVar(&background, "background", false,
		"Launch detached and return a job handle immediately (poll with subagent-status)")
	cmd.Flags().StringVar(&resume, "resume", "",
		"Resume a prior session id for a multi-turn follow-up")
	cmd.Flags().StringVar(&leadSessionID, "lead-session-id", "",
		"Parent Claude session ID for board grouping (optional)")
	cmd.Flags().StringVar(&runID, "run-id", "",
		"Workflow run id to group this job under on the board (optional)")
	cmd.Flags().StringVar(&phase, "phase", "",
		"Workflow phase label within the run (optional)")
	cmd.Flags().StringVar(&label, "label", "",
		"Human label for this agent — shown on the Agents Board instead of the job id (optional)")
	cmd.Flags().StringVar(&promptProfile, "profile", "slim",
		"Prompt profile: slim (default; native generic-subagent mirror) | slim-ro (read-only Explore mirror) | full (the full claude -p session; for behavior comparison)")
	cmd.Flags().StringVar(&tools, "tools", "",
		"Comma/space-separated tool set (slim only; replaces the profile default)")
	cmd.Flags().BoolVar(&skills, "skills", true,
		"Include the Skill tool + host skill listing (slim only; default true, native parity)")
	cmd.Flags().BoolVar(&mcp, "mcp", false,
		"Inherit the host MCP config (slim only; default: slim inherits, slim-ro stays strict)")
	cmd.Flags().BoolVar(&noPersistIO, "no-persist-io", false,
		"Skip persisting the prompt/answer (and live activity) sidecars the board's detail card reads")

	return cmd
}

// splitToolsCSV parses --tools, accepting comma- and/or space-separated names. A blank
// value yields nil; an empty segment inside a non-blank value (e.g. "Read,,Grep" or a
// trailing comma) is an error so a dropped tool can't pass unnoticed. Commas delimit
// segments; whitespace within a segment further splits names ("Read Grep").
func splitToolsCSV(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	for _, seg := range strings.Split(s, ",") {
		names := strings.Fields(seg)
		if len(names) == 0 {
			return nil, fmt.Errorf("empty tool entry in %q", s)
		}
		out = append(out, names...)
	}
	return out, nil
}

// resolveMCPDefault applies the per-profile MCP default when --mcp wasn't
// given: slim inherits the host config, anything else stays strict/inert. An
// explicit --mcp (either value) wins.
func resolveMCPDefault(explicit, flagValue bool, profile string) bool {
	if explicit {
		return flagValue
	}
	return profile == subagent.ProfileSlim
}

// newSubagentStatusCmd builds `cc-fleet subagent-status <job_id>`.
func newSubagentStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "subagent-status <job_id>",
		Short:         "Check a background subagent job (running | done | failed)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportSubagent(subagent.StatusFor(args[0]), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	return cmd
}

// newSubagentGCCmd builds `cc-fleet subagent-gc` — prune finished jobs.
func newSubagentGCCmd() *cobra.Command {
	var (
		asJSON    bool
		olderThan time.Duration
		session   string
	)
	cmd := &cobra.Command{
		Use:           "subagent-gc",
		Short:         "Remove finished background subagent job files",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --session is the granular path: immediately clear that lead session's finished
			// (done/failed/stopped) jobs+runs, skipping pinned. It is status-driven, so an age
			// window doesn't apply; reject --older-than rather than silently ignore it.
			if session != "" {
				if cmd.Flags().Changed("older-than") {
					return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeFailed,
						ErrorMsg: "--session clears finished records immediately; --older-than is not applicable"}, asJSON)
				}
				pins, perr := pinned.Snapshot()
				if perr != nil {
					return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeFailed, ErrorMsg: perr.Error()}, asJSON)
				}
				n, cerr := subagent.ClearFinished(session, pins)
				if cerr != nil {
					return reportSubagent(subagent.Result{OK: false, ErrorCode: subagent.ErrCodeFailed, ErrorMsg: cerr.Error()}, asJSON)
				}
				return reportSubagent(subagent.Result{OK: true, Removed: n}, asJSON)
			}
			return reportSubagent(subagent.GC(olderThan), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a machine-readable JSON envelope")
	cmd.Flags().DurationVar(&olderThan, "older-than", 24*time.Hour,
		"Only GC finished jobs older than this (0s = remove all finished jobs)")
	cmd.Flags().StringVar(&session, "session", "",
		"Clear only this lead session's finished jobs/runs immediately (excludes pinned; --older-than not applicable)")
	return cmd
}

// openPromptFile opens path as the subagent stdin source. "-" means the
// process's own stdin (not closed by us). A real path is opened read-only and
// returned with shouldClose=true. Returning an *os.File (not a generic Reader)
// matters for --background, where the detached child inherits the fd directly.
func openPromptFile(path string) (f *os.File, shouldClose bool, err error) {
	if path == "-" {
		return os.Stdin, false, nil
	}
	file, oerr := os.Open(path)
	if oerr != nil {
		return nil, false, fmt.Errorf("open prompt file %s: %w", path, oerr)
	}
	return file, true, nil
}

// reportSubagent renders a subagent.Result. The three behaviors:
//
//   - --json: marshal exactly ONE Result to stdout, os.Exit(0|1). Bypasses
//     cobra's err echo so JSON consumers see a single envelope.
//   - non-json with a raw payload (bare text answer, or --output-format json
//     inner JSON): write Result.Raw verbatim, exit Result.ExitCode.
//   - non-json structured result (background handle / status / gc) or a
//     front-loaded failure / timeout with no payload: a short human line on
//     stdout (or a one-line stderr note + nonzero exit on failure).
func reportSubagent(res subagent.Result, asJSON bool) error {
	if asJSON {
		data, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintln(os.Stderr, "subagent: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
		if res.OK {
			return nil
		}
		os.Exit(1)
	}

	// Non-JSON. A timeout / exec failure has no useful payload → stderr note.
	if !res.OK && res.ErrorCode == subagent.ErrCodeTimeout {
		fmt.Fprintf(os.Stderr, "subagent: %s: %s\n", res.ErrorCode, res.ErrorMsg)
		os.Exit(1)
	}

	// Raw passthrough: the human/debug paths that asked for claude's own output.
	if len(res.Raw) > 0 {
		_, _ = os.Stdout.Write(res.Raw)
		code := res.ExitCode
		if code < 0 {
			code = 1
		}
		os.Exit(code)
	}

	// No payload: a front-loaded failure (no claude ran).
	if !res.OK {
		fmt.Fprintf(os.Stderr, "subagent: %s: %s\n", res.ErrorCode, res.ErrorMsg)
		if res.Suggestion != "" {
			fmt.Fprintln(os.Stderr, "suggestion:", res.Suggestion)
		}
		os.Exit(1)
	}

	// OK structured result: background handle / status / gc summary.
	switch {
	case res.Status == "running":
		fmt.Printf("subagent job %s: running (pid %d)\n  output: %s\n", res.JobID, res.PID, res.OutputFile)
	case res.Status == "done":
		fmt.Printf("subagent job %s: done\n%s\n", res.JobID, res.Result)
	case res.JobID != "":
		fmt.Printf("subagent job %s: %s\n", res.JobID, res.Status)
	case res.Removed > 0:
		fmt.Printf("subagent-gc: removed %d job file group(s)\n", res.Removed)
	default:
		fmt.Println("subagent: ok")
	}
	return nil
}
