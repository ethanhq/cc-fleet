package subagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/providerclass"
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
// to disk instead). A stream-json stdout keeps only its result line, so there the
// cap bounds one line, never the transcript. Package var so tests can shrink it.
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

// resultLineWriter is the stdout capture of a stream-json run: it keeps only the
// last type:"result" line and drops every other line once complete (each partial
// delta, message and tool result), so a long leaf's transcript is never held. A
// single line over limit trips overflow and onOverflow, as for cappedWriter.
type resultLineWriter struct {
	limit      int
	line       []byte // the line being assembled
	result     []byte
	overflow   bool
	onOverflow func()
}

func (w *resultLineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && !w.overflow {
		chunk := p
		i := bytes.IndexByte(p, '\n')
		if i >= 0 {
			chunk = p[:i+1]
		}
		if len(w.line)+len(chunk) > w.limit {
			w.overflow, w.line = true, nil
			if w.onOverflow != nil {
				w.onOverflow()
			}
			break
		}
		w.line = append(w.line, chunk...)
		p = p[len(chunk):]
		if i >= 0 {
			w.flush()
		}
	}
	return n, nil
}

func (w *resultLineWriter) flush() {
	if isResultLine(w.line) {
		w.result = append(w.result[:0], w.line...)
	}
	w.line = w.line[:0]
}

// detectLeadSession is a seam so tests can inject a parent Claude session
// without relying on the process tree they run under.
var detectLeadSession = leadsession.Detect

// resolveLeadSession applies the launcher-grouping precedence for a job: an
// explicit flag wins, else the parent Claude session (detectLeadSession), else the
// Codex launcher id (CODEX_THREAD_ID) when run from a Codex session, else "" — the
// board's "(no session)".
func resolveLeadSession(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if s := detectLeadSession(); s != "" {
		return s
	}
	return leadsession.CodexThread()
}

// ensureProviderProxy ensures the codex conversion daemon for a codex provider
// (a no-op for every other provider). A package var so tests can stub it without
// launching a real daemon process.
var ensureProviderProxy = codexproxy.EnsureForProvider

// hasReservedRow reports whether providers.toml carries a provider table named
// like the native leaf. Load-independent on purpose: the parsed config when it
// loads, a raw table-header scan when it doesn't — a syntax error elsewhere in
// the file must not disable the reserved-row billing guard. A missing file is
// "no row"; an EXISTING file that cannot be read fails closed (err non-nil) —
// the guard never guesses about a file that is there.
func hasReservedRow() (bool, error) {
	if cfg, err := config.Load(); err == nil {
		_, exists := cfg.Providers[config.ReservedNativeProvider]
		return exists, nil
	}
	path, err := config.ProvidersPath()
	if err != nil {
		return false, nil // no resolvable config location ≡ no file
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return reservedRowRe.Match(raw), nil
}

// reservedRowRe matches a `[claude]` TOML table header at line start, in its
// bare, spaced, or quoted forms ([claude] / [ claude ] / ["claude"] / ['claude']).
var reservedRowRe = regexp.MustCompile(`(?m)^\s*\[\s*['"]?` + config.ReservedNativeProvider + `['"]?\s*\]`)

// proxyPortOf is the loopback conversion-daemon port a daemon-backed provider
// rides, recorded on the job meta so the Windows daemon can count its live
// workers from the job store (process argv is unreadable there). 0 for a
// direct provider or an unparseable base_url.
func proxyPortOf(v *config.Provider) int {
	if v == nil || !v.DaemonBacked() {
		return 0
	}
	u, err := config.ParseLoopbackURL(v.BaseURL)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// Run executes the full subagent pipeline and returns a structured Result. It
// NEVER returns a Go error — every failure path produces a Result.
// Its hard deadline derives from parent (the workflow engine's per-leaf cancel
// handle; the CLI lane passes context.Background()): a cancelled parent kills the
// exec promptly and classifies as a stop, not a failure. nil falls back to Background.
func Run(parent context.Context, req Request) Result {
	// 0. Validate the prompt profile + slim refinements front-loaded, BEFORE any
	//    exec or side effect (mirrors the CLI's front-loaded check; the workflow
	//    engine never reaches here with bad args). Refinements (tools / skills-off
	//    / mcp) are slim-only — combined with the full profile they are rejected.
	if errRes := validateSlimArgs(req); errRes != nil {
		return *errRes
	}
	dg := req.Diag
	eng := defaultEngine

	// Native leaf: the reserved `claude` provider runs the official claude CLI
	// on the user's own login. It has no providers.toml row (and must work even
	// when providers.toml is malformed or absent), no profile, no base URL, no
	// key — steps 1, 2, 4 and 5 below are provider machinery it skips; the
	// child authenticates itself from claude's own credential chain.
	native := req.Provider == config.ReservedNativeProvider
	var v *config.Provider
	var model, effort string
	if native {
		// The slot keywords resolve against a provider roster; the native leaf
		// has none. A literal id passes through; "" omits --model so claude
		// picks the login's own default model.
		switch req.Model {
		case "default", "strong", "fast":
			return fail(ErrCodeBadArgs,
				fmt.Sprintf("model keyword %q needs a provider roster — the native leaf takes a literal model id, or none for the login's default", req.Model),
				req.Provider, "pass a literal id ("+strings.Join(nativeModelAliases, " / ")+" / full id) or omit --model")
		}
		// A pre-reservation providers.toml row named `claude` must not be
		// silently bypassed (the caller configured a backend, a key, a model —
		// rerouting to their subscription is a cost regression): fail with the
		// migration path instead. An unloadable config can't be consulted and
		// must not gate the native leaf — but a raw scan still catches a
		// `[claude]` table inside a malformed file, so a syntax error elsewhere
		// can't disable the billing guard.
		reserved, rerr := hasReservedRow()
		if rerr != nil {
			return fail(ErrCodeFailed,
				fmt.Sprintf("cannot verify providers.toml for a reserved %q row: %v", config.ReservedNativeProvider, rerr),
				req.Provider, "fix the providers.toml read error, then retry")
		}
		if reserved {
			return fail(ErrCodeProviderReserved,
				fmt.Sprintf("providers.toml has a provider named %q, which is reserved for the native leaf", config.ReservedNativeProvider),
				req.Provider, suggestionFor(ErrCodeProviderReserved))
		}
		model = req.Model
	} else {
		// 1. Load provider config.
		cfg, err := config.Load()
		if err != nil {
			return fail(ErrCodeUnknownProvider, fmt.Sprintf("load providers.toml: %v", err),
				req.Provider, suggestionFor(ErrCodeUnknownProvider))
		}
		var ok bool
		v, ok = cfg.Providers[req.Provider]
		if !ok {
			return fail(ErrCodeUnknownProvider, fmt.Sprintf("provider %q not in providers.toml", req.Provider),
				req.Provider, suggestionFor(ErrCodeUnknownProvider))
		}
		if !v.Enabled {
			return fail(ErrCodeProviderDisabled, fmt.Sprintf("provider %q is disabled in providers.toml", req.Provider),
				req.Provider, suggestionFor(ErrCodeProviderDisabled))
		}

		// 2. Resolve model (capability keyword default/strong/fast → slot id,
		//    else a literal id, "" → default_model) and that slot's effort.
		model = v.ResolveModel(req.Model)
		effort = v.ResolveEffort(req.Model)
	}

	// 3. Binary gate: resolve the live claude binary (claudebin — PATH, then the
	//    per-version layout; fingerprint.json is never read, so a CC upgrade can't
	//    leave a stale recorded binary in use). The code stays FINGERPRINT_STALE:
	//    skills and classify dispatch on it; it now means "no claude binary".
	binPath, binVersion, err := eng.Resolve()
	if err != nil {
		dg.Logf("subagent: binary gate: %v", err)
		return fail(ErrCodeFingerprintStale, errMsgNoClaude,
			req.Provider, suggestionFor(ErrCodeFingerprintStale))
	}
	dg.Logf("subagent: binary gate ok (binary %s)", binPath)

	var profilePath string
	if !native {
		// 4. Engine-side provider state (claude: the conversion daemon, then the
		//    profile) — after the binary gate, so a gate failure leaves none behind.
		var failed *Result
		if profilePath, failed = eng.Prepare(v, dg); failed != nil {
			return *failed
		}

		// 5. Optional reachability probe (default OFF). Shares teammate check's classifier;
		//    on Block we abort, on Warn we note and proceed.
		if req.Probe {
			p := providerclass.Reachability(v)
			if p.Warn != "" {
				fmt.Fprint(os.Stderr, p.Warn)
			}
			if p.Block {
				return fail(p.Code, p.Msg, req.Provider, p.Suggestion)
			}
		}
	} else if req.Probe {
		dg.Logf("subagent: probe skipped — the native leaf has no models endpoint")
	}

	// Infer the launcher when no explicit flag was passed — a parent Claude session
	// (Claude Code's ~/.claude/sessions/<pid>.json registry), else the Codex launcher
	// id (CODEX_THREAD_ID) when run from a Codex session, else "(no session)".
	req.LeadSessionID = resolveLeadSession(req.LeadSessionID)

	// 6. Resolve the EFFECTIVE profile (version gate, fail-open to full with a
	//    reason). It uses the version the binary gate resolved for binPath, so the
	//    gate and the exec can't see different executables.
	effective, downgrade := eng.GateProfile(req.PromptProfile, binVersion)

	// 7. Background mode: launch detached, return a job handle.
	if req.Background {
		return launchBackground(eng, req, v, binPath, profilePath, model, effort, effective, downgrade)
	}

	// 8. Synchronous exec with a hard deadline.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	// Mint the job id BEFORE eng.Argv so a slim run can write its
	// <jobID>.slimprompt sidecar and reference it via --system-prompt-file. A workflow leaf
	// passes the id of its queued placeholder so the SAME job flips queued→running→terminal
	// (one file); the bare-CLI path leaves it empty and mints fresh, byte-identical to before.
	// A reused id becomes a filesystem path component, so validate it (the engine always passes a
	// uuid; a malformed/path-unsafe id falls back to a fresh mint rather than escaping the jobs dir).
	jobID := req.JobID
	if jobID == "" || ids.ValidateJobID(jobID) != nil {
		jobID = mintSyncJobID()
	}

	spec := launchSpec{req: req, provider: v, bin: binPath, settings: profilePath,
		model: model, effort: effort, profile: effective, jobID: jobID}
	argv, sidecar, aerr := eng.Argv(spec)
	if aerr != nil {
		res := fail(ErrCodeFailed, aerr.Error(), req.Provider, "")
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		return res
	}
	hostEnv := os.Environ()
	env := eng.Env(hostEnv, spec)
	// Counts only — argv carries the prompt/schema and env carries arbitrary
	// user values, so neither is ever logged.
	dg.Logf("subagent: job %s argv %d args; env %d→%d after cred/marker scrub",
		jobID, len(argv), len(hostEnv), len(env))

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
	//
	// registerHeld means the engine's kill-and-HOLD pre-marked this job and
	// cancelled this very attempt before it registered: the held meta survives
	// untouched, no cache is written, and the attempt exits as the stop the
	// cancel asked for.
	reg := registerSyncJob(jobID, req, model, effective, downgrade, proxyPortOf(v))
	if reg == registerHeld {
		if sidecar != "" {
			_ = os.Remove(sidecar)
		}
		res := fail(ErrCodeStopped, "leaf held while the attempt was starting", req.Provider, "")
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		return res
	}
	registered := reg == registerOK
	var res Result
	if registered {
		// The REAPED finalize: this deferred call runs only after runClaude returns, so the child is
		// provably reaped (or cmd.Start never launched one) — the evidence that clears a false-pending stamp.
		defer func() { finalizeSyncJobReaped(jobID, res) }()
	} else if sidecar != "" {
		defer func() { _ = os.Remove(sidecar) }()
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

	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	// Record the sync leaf's claude CHILD identity once it starts — meta.PID is the engine (a proxy a
	// SIGKILL breaks), so the worktree reclaimers need the child's own pid+token to tell a live orphan
	// from a dead leaf. Capture THIS spawn's attempt so a late write can't stamp a restarted row (the
	// attempt guard). Only for a registered job (a held/failed registration has no meta to update).
	var onStart func(int)
	if registered && jobID != "" {
		attempt := req.Attempt
		onStart = func(childPID int) { recordChildIdentity(jobID, childPID, attempt) }
	}
	stdout, stderr, exitCode, runErr := runClaude(ctx, binPath, argv, env, req.PromptReader, req.WorkingDir, req.StreamActivity, act, onStart)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	dg.Logf("subagent: claude exited code %d (timeout=%v)", exitCode, timedOut)

	// A cancelled parent (the workflow engine aborting its run) is a STOP, not a
	// failure or a timeout — classify it ahead of everything else so the job
	// finalizes "stopped" (the deferred finalizeSyncJob maps ErrCodeStopped).
	if !timedOut && parent.Err() != nil {
		res = fail(ErrCodeStopped, "run stopped while the leaf was executing", req.Provider, "")
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		res.ExitCode = exitCode
		return res
	}

	// A genuine deadline wins over an overflow that fired during the kill (the
	// task ran too long is the dominant cause). Otherwise an over-cap child
	// surfaces as SUBAGENT_OUTPUT_TOO_LARGE — never a misclassified truncation.
	if !timedOut && errors.Is(runErr, errOutputTooLarge) {
		res = fail(ErrCodeOutputTooLarge,
			fmt.Sprintf("provider %s child output exceeded %d bytes", req.Provider, maxChildOutput),
			req.Provider, suggestionFor(ErrCodeOutputTooLarge))
		res.LeadSessionID = req.LeadSessionID
		res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
		res.PromptProfile, res.SlimDowngrade = effective, downgrade
		res.ExitCode = exitCode
		return res
	}

	// 8. Classify into the outer envelope, plus stash the raw passthrough.
	res = eng.Classify(spec, stdout, stderr, exitCode, timedOut)
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
func buildArgv(binaryPath, profilePath, model, effort string, req Request, slim slimArgv) []string {
	argv := []string{binaryPath}

	// Permissions: default to --dangerously-skip-permissions (headless has no
	// TTY to confirm prompts; this is the SAME risk surface as a provider
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

	// A native (reserved `claude`) run has no profile and may have no model:
	// --settings is what injects the provider base URL + apiKeyHelper, so its
	// absence IS the native auth story, and an absent --model lets claude pick
	// the login's own default.
	if profilePath != "" {
		argv = append(argv, "--settings", profilePath)
	}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	argv = append(argv, config.EffortArgs(effort)...)
	argv = append(argv, "-p")
	if req.PromptReader == nil {
		argv = append(argv, req.Prompt)
	}

	switch {
	case req.StreamActivity:
		// stream-json (requires --verbose) so per-message tool_use + usage can be tailed live;
		// --include-partial-messages streams text_delta + a per-message message_delta usage so the
		// token count climbs WITHIN a turn, not only at its end. The terminal type:"result" line is
		// byte-identical to --output-format json for classify.
		argv = append(argv, "--output-format", "stream-json", "--verbose", "--include-partial-messages")
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

	// claude splits the list itself, outside parentheses; re-splitting here
	// would rewrite a rule such as Bash(echo a, b).
	if req.DisallowedTools != "" {
		argv = append(argv, "--disallowedTools", req.DisallowedTools)
	}

	// Slim profiles: replace the main prompt with the rendered native-mirror
	// sidecar, restrict the tool pool, and isolate MCP unless the caller asked to
	// inherit the host config. Thinking is left to the model's config (claude's
	// settings and the slot's effort), as for a native subagent. Appended after
	// the full argv so a full run stays byte-identical.
	if slim.promptFile != "" {
		argv = append(argv, "--system-prompt-file", slim.promptFile,
			"--tools", strings.Join(slim.tools, ","))
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
// With streamJSON the returned stdout is only the result line (resultLineWriter).
func runClaude(ctx context.Context, binaryPath string, argv, env []string, stdin io.Reader, workingDir string, streamJSON bool, act *activityWriter, onStart func(childPID int)) (stdout, stderr []byte, exitCode int, err error) {
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
	resW := &resultLineWriter{limit: maxChildOutput, onOverflow: killGroup}
	var capture io.Writer = outW
	if streamJSON {
		capture = resW
	}
	// Activity capture (opt-in) wraps stdout CAP-FIRST + non-blocking: every write hits the
	// byte-cap first (overflow→kill timing unchanged), then a copy is handed to a parser over a
	// bounded channel that drops on pressure — it can never block the copy goroutine or delay kill.
	var sink *activitySink
	if act != nil {
		sink = newActivitySink(capture, act)
		cmd.Stdout = sink
	} else {
		cmd.Stdout = capture
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
		if onStart != nil {
			onStart(cmd.Process.Pid) // record the CHILD identity (sync leaf's orphan-liveness evidence)
		}
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
	stdout = outW.buf.Bytes()
	if streamJSON {
		resW.flush() // a final line without a trailing newline is still complete
		stdout = resW.result
	}
	if outW.overflow || errW.overflow || resW.overflow {
		return stdout, errW.buf.Bytes(), exitCode, errOutputTooLarge
	}
	return stdout, errW.buf.Bytes(), exitCode, err
}

// SetDetachGroup puts cmd in its own process group (Setpgid on unix,
// CREATE_NEW_PROCESS_GROUP on Windows) — the SAME platform primitive the
// background subagent leaf uses, exported so the workflow runtime can re-exec
// itself as a detached child that outlives the launching CLI without a second,
// divergent platform split. The caller still does Start + Process.Release.
func SetDetachGroup(cmd *exec.Cmd) { setGroupAttr(cmd) }
