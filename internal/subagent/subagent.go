package subagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/vendorclass"
)

// defaultTimeout caps an unset req.Timeout. 300s is deliberately > the ~178s a
// 429 retry can take, so quota exhaustion surfaces as INSUFFICIENT_BALANCE not a timeout.
const defaultTimeout = 300 * time.Second

// waitGrace is how long Go waits after context cancel (SIGTERM via cmd.Cancel)
// before SIGKILLing the child. Package var so tests can shrink it.
var waitGrace = 5 * time.Second

// maxChildOutput bounds each captured child stream (stdout, stderr) on the SYNC
// path. A `claude -p --output-format json` result is KB; the cap only stops a
// runaway child from OOMing the in-memory capture (the --background path streams
// to disk instead). Package var so tests can shrink it.
var maxChildOutput = 32 << 20 // 32 MiB per stream

// errOutputTooLarge is runClaude's sentinel when a captured stream overflowed
// maxChildOutput and the process group was killed. Run maps it to
// SUBAGENT_OUTPUT_TOO_LARGE rather than classifying a truncated body.
var errOutputTooLarge = errors.New("subagent: child output exceeded cap")

// cappedWriter buffers up to limit bytes; the first write that would exceed it
// trips overflow and calls onOverflow (kills the process group), then silently
// discards the rest so the os/exec copy goroutine drains to EOF without an
// EPIPE-driven reclassification. Each instance is written by a single os/exec
// copy goroutine and its fields are read only after cmd.Run() joins that
// goroutine, so it needs no mutex; the shared onOverflow guards itself.
type cappedWriter struct {
	limit      int
	buf        bytes.Buffer
	overflow   bool
	onOverflow func()
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.overflow {
		return len(p), nil // already over: discard, report success
	}
	rem := w.limit - w.buf.Len()
	if len(p) <= rem {
		return w.buf.Write(p)
	}
	if rem > 0 {
		w.buf.Write(p[:rem])
	}
	w.overflow = true
	if w.onOverflow != nil {
		w.onOverflow()
	}
	return len(p), nil // consume the tail into the void
}

// loadFP is a seam so tests can inject a fake fingerprint without a real cache.
// Production = LoadOrBundled: the user's probed cache if present, else the
// bundled default recipe (a fresh install needs no probe).
var loadFP = fingerprint.LoadOrBundled

// LoadFingerprint loads the spawn recipe the same way Run does (probed cache or the
// bundled default). The workflow engine uses it to resolve the effective profile against
// the SAME recipe binary Run will exec, so its pre-keying version gate can't read a
// different executable.
func LoadFingerprint() (*fingerprint.Fingerprint, error) { return loadFP() }

// detectLeadSession is a seam so tests can inject a parent Claude session
// without relying on the process tree they run under.
var detectLeadSession = leadsession.Detect

// Run executes the full subagent pipeline and returns a structured Result. Like
// Spawn it NEVER returns a Go error — every failure path produces a Result.
// It builds its own timeout context (self-contained, like Spawn).
func Run(req Request) Result {
	// 0. Validate the prompt profile + slim refinements front-loaded, BEFORE any
	//    exec or side effect (mirrors the CLI's front-loaded check; the workflow
	//    engine never reaches here with bad args). Refinements (tools / skills-off
	//    / mcp) are slim-only — combined with the full profile they are rejected.
	if errRes := validateSlimArgs(req); errRes != nil {
		return *errRes
	}

	// 1. Load vendor config.
	cfg, err := config.Load()
	if err != nil {
		return fail(ErrCodeUnknownVendor, fmt.Sprintf("load vendors.toml: %v", err),
			req.Vendor, suggestionFor(ErrCodeUnknownVendor))
	}
	v, ok := cfg.Vendors[req.Vendor]
	if !ok {
		return fail(ErrCodeUnknownVendor, fmt.Sprintf("vendor %q not in vendors.toml", req.Vendor),
			req.Vendor, suggestionFor(ErrCodeUnknownVendor))
	}
	if !v.Enabled {
		return fail(ErrCodeVendorDisabled, fmt.Sprintf("vendor %q is disabled in vendors.toml", req.Vendor),
			req.Vendor, suggestionFor(ErrCodeVendorDisabled))
	}

	// 2. Resolve model.
	model := req.Model
	if model == "" {
		model = v.DefaultModel
	}

	// 3. Resolve the spawn recipe (probed fingerprint if present, else bundled
	//    default). Use ONLY the binary path, never fp.Env — it carries the
	//    nested-CC / teams triggers that must be stripped, not re-applied (see childenv.Clean).
	fp, err := loadFP()
	if err != nil {
		// LoadOrBundled never returns ErrNotFound (it falls back to the bundled
		// recipe); a non-nil error here means an existing cache is corrupt.
		return fail(ErrCodeFingerprintMissing, fmt.Sprintf("load fingerprint: %v", err),
			req.Vendor, suggestionFor(ErrCodeFingerprintMissing))
	}
	// Resolve the binary path live (cached-if-exists, else ccver) so a CC
	// upgrade that GC'd the recipe's pinned path doesn't strand us.
	binPath, err := fingerprint.ResolveBinaryPath(fp)
	if err != nil {
		return fail(ErrCodeFingerprintStale, err.Error(),
			req.Vendor, suggestionFor(ErrCodeFingerprintStale))
	}
	fp.BinaryPath = binPath
	// Shared runtime gate — the same helper spawn.Spawn uses, so the two callers
	// can't drift. After dynamic resolution this is defence in depth (the
	// resolved path was just stat-ed) but cheap to keep.
	if err := fingerprint.ValidateForRuntime(fp); err != nil {
		return fail(ErrCodeFingerprintStale,
			err.Error(),
			req.Vendor, suggestionFor(ErrCodeFingerprintStale))
	}

	// 4. Ensure the per-vendor profile exists. Atomic temp+rename + idempotent,
	//    so it's safe with no lock even under N concurrent subagents for one
	//    vendor (the package's lock-free invariant).
	//
	//    MUST run AFTER the fingerprint gate above, not before — fail-before-
	//    side-effects, so a corrupt/missing fingerprint never leaves a profile
	//    file behind. profilePath is only consumed later, so the move is safe.
	profilePath, err := profile.WriteForVendor(v, "")
	if err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("write profile for %s: %v", req.Vendor, err),
			req.Vendor, "")
	}

	// 5. Optional reachability probe (default OFF). Shares spawn's classifier;
	//    on Block we abort, on Warn we note and proceed.
	if req.Probe {
		p := vendorclass.Reachability(v)
		if p.Warn != "" {
			fmt.Fprint(os.Stderr, p.Warn)
		}
		if p.Block {
			return fail(p.Code, p.Msg, req.Vendor, p.Suggestion)
		}
	}

	// Prefer the explicit flag, but when cc-fleet is launched from a Claude Bash
	// tool without a team context, infer the current parent Claude session from
	// Claude Code's own ~/.claude/sessions/<pid>.json registry. Failure is benign:
	// the job remains in the legacy "(no session)" board bucket.
	if req.LeadSessionID == "" {
		req.LeadSessionID = detectLeadSession()
	}

	// 6. Resolve the EFFECTIVE profile (version gate, fail-open to full with a
	//    reason). Done AFTER the fingerprint gate, against the SAME fp whose binary
	//    path was just resolved above — no second fingerprint load, so the gate can't
	//    read a different executable than the one this Run will exec.
	effective, downgrade := ResolveEffectiveProfile(req.PromptProfile, fp)

	// 7. Background mode: launch detached, return a job handle.
	if req.Background {
		return launchBackground(req, fp.BinaryPath, profilePath, model, effective, downgrade)
	}

	// 8. Synchronous exec with a hard deadline.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	// Mint the job id BEFORE buildArgv so a slim run can write its
	// <jobID>.slimprompt sidecar and reference it via --system-prompt-file. A workflow leaf
	// passes the id of its queued placeholder so the SAME job flips queued→running→terminal
	// (one file); the bare-CLI path leaves it empty and mints fresh, byte-identical to before.
	// A reused id becomes a filesystem path component, so validate it (the engine always passes a
	// uuid; a malformed/path-unsafe id falls back to a fresh mint rather than escaping the jobs dir).
	jobID := req.JobID
	if jobID == "" || ids.ValidateJobID(jobID) != nil {
		jobID = mintSyncJobID()
	}

	slim, slimErr := buildSlimArgv(effective, jobID, req, model)
	if slimErr != nil {
		res := fail(ErrCodeFailed, slimErr.Error(), req.Vendor, "")
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		return res
	}
	argv := buildArgv(fp.BinaryPath, profilePath, model, req, slim)
	env := childenv.Clean(os.Environ())
	if effective == ProfileSlimRO {
		env = append(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
	}

	// Register this run on the Agents Board so a sync subagent is visible
	// WHILE it runs, then flip it to done/failed on return via a deferred
	// sanitized result cache. Done-detection rides the cache, NOT pid liveness —
	// the recorded pid is this cc-fleet process and gets recycled once it exits.
	// The returned res is unchanged (no JobID stamped), so CLI output is
	// identical; board bookkeeping is purely a side channel.
	//
	// When registration FAILS (no meta on disk) finalizeSyncJob is skipped — it
	// would otherwise write an orphan .result.json with no backing meta — and a
	// slim sidecar already written by buildSlimArgv is reaped after the child
	// exits, since GC keys on the (absent) meta and would never find it.
	registered := registerSyncJob(jobID, req, model, effective, downgrade)
	var res Result
	if registered {
		defer func() { finalizeSyncJob(jobID, res) }()
	} else if slim.promptFile != "" {
		defer func() { _ = os.Remove(slim.promptFile) }()
	}

	// Capture per-leaf tool/usage activity to <jobID>.activity (stream-json) when the workflow
	// engine opted in — content-privacy, gated like the prompt/answer side files. Skipped when
	// registration failed: with no meta the .activity file would orphan exactly like the cache.
	var act *activityWriter
	if registered && req.StreamActivity && jobID != "" {
		if p, perr := leafActivityPath(jobID); perr == nil {
			act = newActivityWriter(p)
			act.inputSeed = estimatePromptTokens(req.IOPrompt) // live input floor until real usage arrives
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stdout, stderr, exitCode, runErr := runClaude(ctx, fp.BinaryPath, argv, env, req.PromptReader, req.WorkingDir, act)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	// A genuine deadline wins over an overflow that fired during the kill (the
	// task ran too long is the dominant cause). Otherwise an over-cap child
	// surfaces as SUBAGENT_OUTPUT_TOO_LARGE — never a misclassified truncation.
	if !timedOut && errors.Is(runErr, errOutputTooLarge) {
		res = fail(ErrCodeOutputTooLarge,
			fmt.Sprintf("vendor %s child output exceeded %d bytes", req.Vendor, maxChildOutput),
			req.Vendor, suggestionFor(ErrCodeOutputTooLarge))
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		res.ExitCode = exitCode
		return res
	}

	// 8. Classify into the outer envelope, plus stash the raw passthrough. A stream-json run is
	//    inner-JSON: classify the single terminal type:"result" line (byte-identical to the
	//    --output-format json envelope), not the whole multi-line transcript.
	innerJSON := req.JSON || req.OutputFormat == "json" || req.StreamActivity
	classifyOut := stdout
	if req.StreamActivity {
		classifyOut = extractResultLine(stdout)
	}
	res = classify(req, model, classifyOut, stderr, exitCode, timedOut, innerJSON)
	res.LeadSessionID = req.LeadSessionID
	res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
	res.PromptProfile, res.SlimDowngrade = effective, downgrade
	res.Raw = stdout
	res.ExitCode = exitCode
	return res
}

// buildArgv assembles the exact claude argv. It is NOT shell — exec runs it as
// an argv slice, so no quoting is needed. argv[0] is binaryPath.
//
// When PromptReader is set (--prompt-file / stdin) we emit "-p" with NO value
// so claude reads the prompt from stdin and the prompt never enters argv.
//
// slim describes the slim-profile additions and is the empty zero value for a
// full run, which keeps full's argv byte-identical to before. Its flags are
// APPENDED after the full argv (claude is order-insensitive for them).
func buildArgv(binaryPath, profilePath, model string, req Request, slim slimArgv) []string {
	argv := []string{binaryPath}

	// Permissions: default to --dangerously-skip-permissions (headless has no
	// TTY to confirm prompts; this is the SAME risk surface as a vendor
	// teammate, not a new one). A caller wanting a sandbox passes
	// --permission-mode plan|acceptEdits|default.
	if req.PermissionMode != "" {
		argv = append(argv, "--permission-mode", req.PermissionMode)
	} else {
		argv = append(argv, "--dangerously-skip-permissions")
	}

	// Multi-turn: load a prior headless session before this turn.
	if req.Resume != "" {
		argv = append(argv, "--resume", req.Resume)
	}

	argv = append(argv, "--settings", profilePath, "--model", model, "-p")
	if req.PromptReader == nil {
		argv = append(argv, req.Prompt)
	}

	switch {
	case req.StreamActivity:
		// stream-json (requires --verbose) so per-message tool_use + usage can be tailed live;
		// the terminal type:"result" line is byte-identical to --output-format json for classify.
		argv = append(argv, "--output-format", "stream-json", "--verbose")
	case req.JSON || req.OutputFormat == "json":
		argv = append(argv, "--output-format", "json")
	}
	if req.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if req.MaxBudgetUSD > 0 {
		argv = append(argv, "--max-budget-usd", strconv.FormatFloat(req.MaxBudgetUSD, 'f', -1, 64))
	}

	// --json-schema makes claude inject a forced StructuredOutput tool whose
	// input_schema is this schema. Profile-independent; the injected tool
	// survives a slim --tools whitelist.
	if req.JSONSchema != "" {
		argv = append(argv, "--json-schema", req.JSONSchema)
	}

	// Slim profiles: replace the main prompt with the rendered native-mirror
	// sidecar, restrict the tool pool, disable thinking (native subagent
	// behavior), and isolate MCP unless the caller asked to inherit the host
	// config. Appended after the full argv so a full run stays byte-identical.
	if slim.promptFile != "" {
		argv = append(argv, "--system-prompt-file", slim.promptFile,
			"--tools", strings.Join(slim.tools, ","),
			"--thinking", "disabled")
		if !req.MCP {
			argv = append(argv, "--strict-mcp-config")
		}
	}
	return argv
}

// slimArgv carries the slim-profile additions buildArgv appends. promptFile is
// the absolute <jobID>.slimprompt sidecar path; empty means "full profile, no
// slim flags". tools is the canonicalized (deduped + sorted) tool set.
type slimArgv struct {
	promptFile string
	tools      []string
}

// runClaude execs the headless child with a process-group kill model so a
// timeout reaps the WHOLE tree (claude forks Bash-tool grandchildren). It is a
// standalone func so tests can drive it with a fake binary. It never streams to
// the parent's stdio: stdout/stderr are captured to byte-capped buffers, and a
// stream that overflows maxChildOutput kills the group and returns errOutputTooLarge.
func runClaude(ctx context.Context, binaryPath string, argv, env []string, stdin io.Reader, workingDir string, act *activityWriter) (stdout, stderr []byte, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Args = argv // argv[0] == binaryPath by construction
	cmd.Env = env
	cmd.Dir = workingDir // empty = inherit cwd; set for git-worktree isolation
	if stdin != nil {
		cmd.Stdin = stdin
	}

	// The process-group controller owns the whole-tree kill model: a kernel
	// process group on unix (Setpgid → -pid signals reach Bash-tool
	// grandchildren), a Job Object on Windows (the child + every descendant are
	// killed atomically when the job is terminated).
	pg := newProcGroup()
	// Release the group controller on EVERY return path. On Windows this closes the
	// Job Object handle (a no-op once killGroupHard already terminated+closed it on a
	// timeout/overflow path); on unix it is a no-op. Without it the normal-exit path
	// would leak the Windows job handle + its kernel object.
	defer pg.close()

	// Capture each stream through a byte cap so a runaway child can't OOM the
	// parent. On overflow we hard-kill the whole group/tree (the over-cap output
	// is already useless) and surface errOutputTooLarge — never a silent
	// truncation, which would mis-parse into SUBAGENT_FAILED or echo a truncated
	// answer.
	var killOnce sync.Once
	killGroup := func() {
		killOnce.Do(func() {
			if cmd.Process != nil {
				pg.killGroupHard(cmd.Process.Pid)
			}
		})
	}
	outW := &cappedWriter{limit: maxChildOutput, onOverflow: killGroup}
	errW := &cappedWriter{limit: maxChildOutput, onOverflow: killGroup}
	// Activity capture (opt-in) wraps stdout CAP-FIRST + non-blocking: every write hits the
	// byte-cap first (overflow→kill timing unchanged), then a copy is handed to a parser over a
	// bounded channel that drops on pressure — it can never block the copy goroutine or delay kill.
	var sink *activitySink
	if act != nil {
		sink = newActivitySink(outW, act)
		cmd.Stdout = sink
	} else {
		cmd.Stdout = outW
	}
	cmd.Stderr = errW

	// Make the child the group/tree root (Setpgid on unix; CREATE_NEW_PROCESS_GROUP
	// on Windows).
	setGroupAttr(cmd)
	// On context cancel, terminate the whole group (not just the leader). The
	// unix path treats "already gone" (ESRCH) as success so an exit/deadline race
	// doesn't make os/exec think Cancel failed; the Windows path is best-effort
	// graceful with the authoritative reap deferred to the post-Run escalation.
	cmd.Cancel = func() error {
		return pg.signalGroupTerm(cmd.Process.Pid)
	}
	// After this grace window os/exec SIGKILLs/terminates only the leader; we
	// escalate to the whole group/tree below to catch grandchildren that ignored
	// the graceful terminate.
	cmd.WaitDelay = waitGrace

	// Start + afterStart + Wait is semantically identical to cmd.Run() (Run is
	// exactly Start followed by Wait, and Cancel/WaitDelay are honored the same
	// way), but the explicit Start gives the Windows port its assign-after-Start
	// window to bind the leader to the Job Object before it forks children. On a
	// Start failure (err set, cmd.Process nil) the tail below behaves exactly as
	// the old cmd.Run() error path: exitCode -1, no escalation, empty captures.
	if err = cmd.Start(); err == nil {
		pg.afterStart(cmd)
		err = cmd.Wait()
	}

	// When the deadline/cancel fired, Go's WaitDelay reaps only cmd.Process (the
	// leader). A grandchild that trapped/ignored the graceful terminate can
	// survive as an orphan. Escalate to the whole group/tree so no ghosts survive
	// (unix: Kill(-pid, SIGKILL); Windows: TerminateJobObject). An already-empty
	// group is fine.
	if ctx.Err() != nil && cmd.Process != nil {
		pg.killGroupHard(cmd.Process.Pid)
	}
	// Stop + flush the activity parser AFTER cmd.Wait joined the copy goroutine (every captured
	// byte was tee'd); a no-op when there was no sink.
	if sink != nil {
		sink.close()
	}

	exitCode = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode() // -1 if killed by signal
		} else {
			exitCode = -1
		}
	}
	if outW.overflow || errW.overflow {
		return outW.buf.Bytes(), errW.buf.Bytes(), exitCode, errOutputTooLarge
	}
	return outW.buf.Bytes(), errW.buf.Bytes(), exitCode, err
}

// SetDetachGroup puts cmd in its own process group (Setpgid on unix,
// CREATE_NEW_PROCESS_GROUP on Windows) — the SAME platform primitive the
// background subagent leaf uses, exported so the workflow runtime can re-exec
// itself as a detached child that outlives the launching CLI without a second,
// divergent platform split. The caller still does Start + Process.Release.
func SetDetachGroup(cmd *exec.Cmd) { setGroupAttr(cmd) }
