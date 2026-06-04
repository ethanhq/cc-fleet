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
	"sync"
	"syscall"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
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

// detectLeadSession is a seam so tests can inject a parent Claude session
// without relying on the process tree they run under.
var detectLeadSession = leadsession.Detect

// Run executes the full subagent pipeline and returns a structured Result. Like
// Spawn it NEVER returns a Go error — every failure path produces a Result.
// It builds its own timeout context (self-contained, like Spawn).
func Run(req Request) Result {
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

	// 6. Background mode: launch detached, return a job handle.
	if req.Background {
		return launchBackground(req, fp.BinaryPath, profilePath, model)
	}

	// 7. Synchronous exec with a hard deadline.
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	argv := buildArgv(fp.BinaryPath, profilePath, model, req)
	env := childenv.Clean(os.Environ())

	// Register this run on the agent-status board so a sync subagent is visible
	// WHILE it runs, then flip it to done/failed on return via a deferred
	// sanitized result cache. Done-detection rides the cache, NOT pid liveness —
	// the recorded pid is this cc-fleet process and gets recycled once it exits.
	// The returned res is unchanged (no JobID stamped), so CLI output is
	// identical; board bookkeeping is purely a side channel.
	jobID := registerSyncJob(req, model)
	var res Result
	defer func() { finalizeSyncJob(jobID, res) }()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stdout, stderr, exitCode, runErr := runClaude(ctx, fp.BinaryPath, argv, env, req.PromptReader)
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
		res.ExitCode = exitCode
		return res
	}

	// 8. Classify into the outer envelope, plus stash the raw passthrough.
	innerJSON := req.JSON || req.OutputFormat == "json"
	res = classify(req, model, stdout, stderr, exitCode, timedOut, innerJSON)
	res.LeadSessionID = req.LeadSessionID
	res.RunID, res.Phase, res.Label = req.RunID, req.Phase, req.Label
	res.Raw = stdout
	res.ExitCode = exitCode
	return res
}

// buildArgv assembles the exact claude argv. It is NOT shell — exec runs it as
// an argv slice, so no quoting is needed. argv[0] is binaryPath.
//
// When PromptReader is set (--prompt-file / stdin) we emit "-p" with NO value
// so claude reads the prompt from stdin and the prompt never enters argv.
func buildArgv(binaryPath, profilePath, model string, req Request) []string {
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

	if req.JSON || req.OutputFormat == "json" {
		argv = append(argv, "--output-format", "json")
	}
	if req.MaxTurns > 0 {
		argv = append(argv, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if req.MaxBudgetUSD > 0 {
		argv = append(argv, "--max-budget-usd", strconv.FormatFloat(req.MaxBudgetUSD, 'f', -1, 64))
	}
	return argv
}

// runClaude execs the headless child with a process-group kill model so a
// timeout reaps the WHOLE tree (claude forks Bash-tool grandchildren). It is a
// standalone func so tests can drive it with a fake binary. It never streams to
// the parent's stdio: stdout/stderr are captured to byte-capped buffers, and a
// stream that overflows maxChildOutput kills the group and returns errOutputTooLarge.
func runClaude(ctx context.Context, binaryPath string, argv, env []string, stdin io.Reader) (stdout, stderr []byte, exitCode int, err error) {
	cmd := exec.CommandContext(ctx, binaryPath)
	cmd.Args = argv // argv[0] == binaryPath by construction
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = stdin
	}
	// Capture each stream through a byte cap so a runaway child can't OOM the
	// parent. On overflow we SIGKILL the whole group (the over-cap output is
	// already useless) and surface errOutputTooLarge — never a silent truncation,
	// which would mis-parse into SUBAGENT_FAILED or echo a truncated answer.
	var killOnce sync.Once
	killGroup := func() {
		killOnce.Do(func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		})
	}
	outW := &cappedWriter{limit: maxChildOutput, onOverflow: killGroup}
	errW := &cappedWriter{limit: maxChildOutput, onOverflow: killGroup}
	cmd.Stdout = outW
	cmd.Stderr = errW

	// Setpgid makes the child its own group leader, so -Pid == the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On context cancel, SIGTERM the whole group (not just the leader). Treat
	// "already gone" (ESRCH) as success so an exit/deadline race doesn't make
	// os/exec think Cancel failed.
	cmd.Cancel = func() error {
		if e := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); e != nil && !errors.Is(e, syscall.ESRCH) {
			return e
		}
		return nil
	}
	// After this grace window os/exec SIGKILLs only the leader; we escalate to
	// the group below to catch grandchildren that ignored SIGTERM.
	cmd.WaitDelay = waitGrace

	err = cmd.Run()

	// When the deadline/cancel fired, Go's WaitDelay SIGKILLs only cmd.Process
	// (the leader). A grandchild that trapped/ignored SIGTERM can survive as an
	// orphan. Escalate to the whole process group so no ghosts survive. ESRCH
	// (group already empty) is fine.
	if ctx.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
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
