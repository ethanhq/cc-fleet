// Package workflow is cc-fleet's deterministic orchestration runtime: it runs a
// JavaScript workflow script that fans out provider subagent leaves, in a cc-fleet
// process OFF the main Claude context. The script's plan lives in script variables
// (CPU, ~0 tokens); the model is invoked only at agent() leaves. The API mirrors the
// native Claude Code Workflow tool (const meta / agent / parallel / pipeline / phase /
// log / budget / args / workflow); the only shape difference is agent()'s required
// opts.provider.
//
// Concurrency is a single-owner loop (see loop.go): one goroutine runs ALL script
// execution and engine-state mutation, builtins return Promises, and the blocking
// provider execs run on leaf goroutines — so the engine is -race clean while the slow
// leaves still overlap up to a bounded pool.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dop251/goja"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// runLeaf is the provider-subagent leaf — a seam so tests inject a deterministic fake
// in place of a real `claude -p` exec. Production = subagent.Run (in-process,
// key-safe via apiKeyHelper, board-registered, tagged with run/phase/label). The ctx
// is the engine's per-leaf cancel handle: an aborting run cancels it and the exec
// dies promptly instead of draining to its own timeout.
var runLeaf = subagent.Run

// mintQueuedLeaf records a leaf's queued placeholder (PID=0) before it gets a pool slot — a seam so
// the fake-leaf unit tests opt out of the disk side effect by stubbing it to return "". Production =
// subagent.MintQueuedLeaf; the engine reuses the returned id as Request.JobID (queued→running flip).
var mintQueuedLeaf = subagent.MintQueuedLeaf

// resolveProfile maps a REQUESTED prompt profile to the effective one (the version
// gate). A seam so tests drive the downgrade path without a real claude binary.
// Production loads the fingerprint the same way Run does and resolves the version
// against THAT recipe's binary; a load failure fails open to full with a reason,
// matching the resolver's own discipline. Called at most ONCE per engine (see
// engine.effProfileFor), never per leaf.
var resolveProfile = func(requested string) (string, string) {
	if requested == "" || requested == subagent.ProfileFull {
		return requested, ""
	}
	fp, err := subagent.LoadFingerprint()
	if err != nil {
		return subagent.ProfileFull, fmt.Sprintf("slim disabled: load fingerprint: %v", err)
	}
	return subagent.ResolveEffectiveProfile(requested, fp)
}

// Options configures a workflow run.
type Options struct {
	// RunID, when set, names an EXISTING manifest to execute (the detached child /
	// foreground re-exec path); empty means Prepare mints a fresh one.
	RunID       string
	Concurrency int    // 0 → defaultConcurrency()
	ArgsJSON    string // optional; the script's `args` value
	// Resume names an EXISTING run to re-execute against its journal (cross-invocation
	// replay): Launch reuses this id instead of minting a fresh one, and the engine's
	// unconditional journal load then serves the leaves that already completed. Empty =
	// a fresh run. Used only at Launch — the detached child carries the id via RunID and
	// resumes transparently (the journal load keys on the id, not this flag).
	Resume string
	// NoPersistIO opts OUT of the board's inline prompt/answer detail (persistence is DEFAULT ON).
	// When set, leaves persist no <jobID>.prompt/.answer side files; the result cache is
	// answer-stripped either way, so the board table is unaffected. Propagated to the
	// detached child so the whole run honors it.
	NoPersistIO bool
	// BudgetUSD caps total provider spend in USD (a sum of leaf CostUSD — an Anthropic list-price
	// estimate, not the third-party provider's actual charge); agent() raises once the cap would be
	// breached. <=0 is uncapped; a -1 sentinel from --no-budget is normalized to 0 (explicit uncap)
	// in Launch before any persist. Propagated to the detached child.
	BudgetUSD float64
	// BudgetTokens caps total provider token spend (Usage.InputTokens+OutputTokens summed across
	// leaves, cache-read excluded — the exact provider-neutral ceiling); <=0 uncapped, -1 normalized
	// to 0 like BudgetUSD. The first cap to trip aborts. Propagated to the detached child.
	BudgetTokens int64
	// LeadSessionID is the parent Claude session this run was launched from (detected at
	// `workflow run`). Stored on the manifest at mint so the board groups runs by session.
	// Used only by Launch's fresh-mint path; a resume reads it back off the manifest.
	LeadSessionID string
}

// Prepare parses a script, extracts + validates its `const meta` literal, and mints a
// run manifest with the name/description/declared phases — BEFORE any execution, so a
// bad script never mints a half-run and the board shows the named, phase-skeletoned
// run immediately. The normalize step full-parses the wrapped body, so any syntax
// error fails here with NO manifest left behind. Returns the new manifest (its RunID
// is handed to a detached child or printed to the caller).
func Prepare(scriptPath string) (subagent.WorkflowRun, error) {
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return subagent.WorkflowRun{}, fmt.Errorf("workflow: read script: %w", err)
	}
	_, prog, nerr := normalizeScript(scriptPath, src)
	if nerr != nil {
		return subagent.WorkflowRun{}, nerr
	}
	meta, merr := extractMeta(prog)
	if merr != nil {
		return subagent.WorkflowRun{}, merr
	}
	return subagent.NewRunWithMeta(meta.Name, meta.Description, meta.WhenToUse, metaPhases(meta))
}

// metaPhases converts a parsed script meta's phase plan into the manifest's RunPhase
// slice. Both Prepare (minting the manifest) and Execute (re-deriving the engine's
// authoritative copy) feed it, so the on-disk phase shape stays identical between them.
func metaPhases(meta scriptMeta) []subagent.RunPhase {
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}
	return phases
}

// Execute runs a prepared script's body to completion in the CURRENT process,
// tagging every leaf with runID, and flips the manifest to done/failed — or "stopped"
// when the run ctx was cancelled (a cooperative stop is not a failure) — on exit. It
// NEVER lets a panic escape: a panic is recovered into a failed status so a detached
// run always finalizes.
func Execute(ctx context.Context, scriptPath, runID string, opts Options) (err error) {
	// Fail-fast on a bad run id (e.g. a malformed `--run-id`) before doing anything —
	// it becomes a manifest path component, and a doomed-to-not-persist run shouldn't run.
	if verr := subagent.ValidateRunID(runID); verr != nil {
		return fmt.Errorf("workflow: invalid run id: %w", verr)
	}
	// Normalize the -1 uncap sentinel here too, not only in Launch: the detached `--run-id` re-exec
	// reaches Execute directly, so the engine and its manifest writes never see a negative budget.
	normalizeBudgetSentinels(&opts)
	// Seed the engine's authoritative manifest state from the Prepare-minted manifest
	// (best-effort) + the script's meta. The engine then OWNS the manifest, overwriting
	// it whole on every phase()/finalize — so there is no read-modify-write to race and
	// a concurrently-dropped manifest is recreated on the next write.
	prepared, _ := subagent.ReadRun(runID) // the minted manifest, if still present
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if prepared.StartedAt != "" {
		startedAt = prepared.StartedAt
	}
	// On a pre-execution failure, finalize the run as failed WITHOUT dropping the
	// prepared name/description/phases (only status + the failure cause change).
	failManifest := func(cause error) {
		prepared.RunID = runID
		if prepared.StartedAt == "" {
			prepared.StartedAt = startedAt
		}
		prepared.Status = "failed"
		prepared.Error = cause.Error()
		_ = subagent.SaveRun(prepared)
	}

	src, rerr := os.ReadFile(scriptPath)
	if rerr != nil {
		e := fmt.Errorf("workflow: read script: %w", rerr)
		failManifest(e)
		return e
	}
	normalized, prog, nerr := normalizeScript(scriptPath, src)
	if nerr != nil {
		failManifest(nerr)
		return nerr
	}
	meta, merr := extractMeta(prog)
	if merr != nil {
		failManifest(merr)
		return merr
	}
	phases := metaPhases(meta)

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency()
	}
	// leafCtx is every leaf exec's parent: a cooperative stop (ctx) or an aborting run
	// (script failure) cancels it, and the in-flight execs die promptly.
	leafCtx, cancelLeaves := context.WithCancel(ctx)
	defer cancelLeaves()
	eng := &engine{
		sched: newScheduler(concurrency), runID: runID,
		runCtx: ctx, leafCtx: leafCtx, cancelLeaves: cancelLeaves,
		cbs: make(chan leafCB, 64), loopDone: make(chan struct{}), ctl: map[string]*leafCtl{}, heldPhases: map[string]bool{},
		name: meta.Name, description: meta.Description, startedAt: startedAt, phases: phases,
		persistIO:          !opts.NoPersistIO, // the board's inline prompt/answer detail is default-on
		enginePID:          detachedEnginePID(opts),
		metaModel:          meta.Model, // default model for agents that omit model
		whenToUse:          meta.WhenToUse,
		budgetTotal:        opts.BudgetUSD,
		budgetTokensTotal:  opts.BudgetTokens,
		sessionID:          prepared.SessionID, // from the minted manifest; re-persisted on every save
		cwd:                prepared.Cwd,       // launching project dir; ditto
		argsJSON:           opts.ArgsJSON,
		defaultProvider:    prepared.DefaultProvider,      // resolved at mint; re-persisted on every save
		defaultProviderErr: prepared.DefaultProviderError, // the code a provider-less agent() throws
	}
	// Load the run's content-hash journal (resume). The path is derived from the
	// already-validated runID; a missing file (a fresh run) yields an empty cache that
	// the first completed leaf creates. The load is unconditional, so a detached
	// --run-id child resumes transparently with no extra flag.
	//
	// Concurrency note: two simultaneous resumes of the SAME run id are not serialized
	// here — each loads the journal, runs un-cached leaves, and (atomically, O_APPEND
	// per line) appends; the worst case is duplicated leaf work, not a corrupt journal
	// or manifest. There is no per-run execution lock, so a caller must not launch
	// concurrent resumes of one id.
	if jp, jerr := subagent.RunJournalPath(runID); jerr == nil {
		eng.journal = loadJournal(jp)
	}
	// Open the run's live-event channel that `workflow watch` renders. Best-effort: a path
	// error leaves events nil → emits no-op. The file is TRUNCATED at the start of every
	// invocation (incl. a resume), so the live stream always represents exactly the CURRENT
	// invocation rather than a concatenation of per-invocation event streams (it would otherwise
	// double on resume). Safe because events are observability only — never read back by the
	// engine, never journaled.
	if ep, eerr := subagent.RunEventsPath(runID); eerr == nil {
		_ = os.Remove(ep)
		eng.events = newEventWriter(ep)
	}
	// A prior invocation killed mid-hold leaves `held` metas behind (no abort walk ran);
	// normalize them BEFORE this invocation runs — holds are never persisted.
	subagent.NormalizeStaleHolds(runID)
	// The control plane: truncate stale directives BEFORE the manifest says `running`
	// (a writer gates on that status — commands meant for the prior invocation must
	// never apply to this one, and ones issued for THIS one must never be lost), then
	// stamp, then start the poller.
	ctlPath, ctlErr := subagent.RunCtlPath(runID)
	if ctlErr == nil {
		_ = os.Remove(ctlPath)
	}
	// Stamp the manifest once up front: records EnginePID (so `workflow stop` can reap
	// this process), flips a resumed run to "running", and refreshes UpdatedAt from the
	// start (GC recency) — before the first leaf, closing the mint→first-leaf window.
	eng.saveManifest("running", "")
	if ctlErr == nil {
		eng.startCtlPoller(ctlPath)
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workflow: script panicked: %v", r)
		}
		status, errText := "done", ""
		switch {
		case eng.stopped:
			status = "stopped" // a cooperative stop is not a failure; Error stays empty
		case err != nil:
			status, errText = "failed", err.Error()
		}
		eng.saveManifest(status, errText)
	}()

	_, execErr := eng.run(scriptPath, normalized, opts)
	return execErr
}

// detachedEnginePID is os.Getpid() for the DETACHED engine child (opts.RunID set by the
// re-exec) and 0 for a foreground/inline run. `workflow stop` reaps only a recorded
// (detached) engine pid; a foreground run is stopped in its own terminal (Ctrl-C), so it
// records no pid rather than letting stop claim a kill it can't make.
func detachedEnginePID(opts Options) int {
	if opts.RunID != "" {
		return os.Getpid()
	}
	return 0
}

// run executes a normalized script body: it builds the VM, compiles the wrapped form,
// calls the body arrow with (workflow, args), and drives the loop until the returned
// async-IIFE promise settles and the in-flight leaves drain. Returns the script's
// settled value. Shared by Execute and the tests (which assert on the value's Export).
func (e *engine) run(scriptPath string, src []byte, opts Options) (goja.Value, error) {
	if err := e.setupVM(); err != nil {
		return nil, err
	}
	// loopDone marks the loop's end of life on EVERY exit path: it unblocks straggler
	// post()s and retires the watchdog (an early return that skipped it would leak both).
	defer close(e.loopDone)
	// Interrupt any JS busy on the loop when the run ctx dies — the cooperative-stop
	// path for a script spinning in pure JS (the loop's select can't see ctx while a
	// callback runs). The watchdog dies with the loop.
	go func() {
		select {
		case <-e.runCtx.Done():
			e.vm.Interrupt("workflow: run stopped")
		case <-e.loopDone:
		}
	}()
	prog, cerr := goja.Compile(scriptPath, wrapScript(src), false)
	if cerr != nil {
		return nil, cerr
	}
	fnVal, xerr := e.vm.RunProgram(prog)
	if xerr != nil {
		return nil, xerr
	}
	fn, ok := goja.AssertFunction(fnVal)
	if !ok {
		return nil, fmt.Errorf("workflow: script did not compile to a callable body")
	}
	pv, callErr := fn(goja.Undefined(), e.workflowFn, e.argsValue(opts))
	if callErr != nil {
		// An async body converts a sync throw into a rejection, so an error here is
		// uncatchable — an Interrupt from the watchdog means the run was stopped. The
		// body may have spawned leaves before dying mid-statement: drain them so their
		// jobs finalize (stopped-class) instead of stranding as queued.
		if e.runCtx.Err() != nil {
			e.stopped = true
		}
		e.drainLeaves()
		return nil, callErr
	}
	prom, ok := pv.Export().(*goja.Promise)
	if !ok {
		e.drainLeaves()
		return nil, fmt.Errorf("workflow: script body did not produce a promise")
	}
	return e.drive(prom)
}

// argsValue materializes --args-json as a plain JS value via the VM's own JSON.parse
// (never a Go host-object wrapper). Absent — or invalid, though Launch pre-validates —
// yields undefined, so a script reading `args` sees undefined rather than throwing.
func (e *engine) argsValue(opts Options) goja.Value {
	if opts.ArgsJSON == "" {
		return goja.Undefined()
	}
	v, err := e.jsonParse(goja.Undefined(), e.vm.ToValue(opts.ArgsJSON))
	if err != nil {
		return goja.Undefined()
	}
	return v
}

// normalizeBudgetSentinels turns the --no-budget -1 sentinel into 0 (explicit uncap) for both caps,
// so the engine and the manifest only ever see >=0 (0 = uncapped). Called AFTER the resume-inherit
// decision and before any persist, so -1 never inherits the old cap and is never written.
func normalizeBudgetSentinels(opts *Options) {
	if opts.BudgetUSD < 0 {
		opts.BudgetUSD = 0
	}
	if opts.BudgetTokens < 0 {
		opts.BudgetTokens = 0
	}
}

// Launch is the entry for `cc-fleet workflow run`. It prepares the run (parse + meta +
// mint), then either runs it inline (foreground — the debug / deterministic-e2e path)
// or re-execs cc-fleet as a DETACHED child that runs it to completion off the launching
// process, returning the run id immediately. Detaching reuses the subagent leaf's
// proven process-group primitive — no new platform split.
// resolveRunDefault resolves the run-level default provider at mint, returning
// EITHER the provider name OR a stable error_code (never both). A config-load
// failure or an unresolvable default yields the code; an all-explicit script
// never consumes it. Used by Launch.
func resolveRunDefault() (provider, errCode string) {
	cfg, err := config.Load()
	if err != nil {
		return "", config.ProviderErrorCode(err)
	}
	name, _, rerr := cfg.ResolveProvider("")
	if rerr != nil {
		return "", config.ProviderErrorCode(rerr)
	}
	return name, ""
}

func Launch(ctx context.Context, scriptPath string, opts Options, foreground bool) (string, error) {
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		return "", fmt.Errorf("workflow: resolve script path: %w", err)
	}
	if opts.ArgsJSON != "" && !json.Valid([]byte(opts.ArgsJSON)) {
		return "", fmt.Errorf("workflow: --args-json is not valid JSON")
	}
	// Resume reuses an existing run id (validated + confirmed to exist) so the engine
	// replays its journal; a fresh run mints a new manifest from the script's meta.
	var run subagent.WorkflowRun
	if opts.Resume != "" {
		if verr := subagent.ValidateRunID(opts.Resume); verr != nil {
			return "", fmt.Errorf("workflow: invalid resume id: %w", verr)
		}
		existing, rerr := subagent.ReadRun(opts.Resume)
		if rerr != nil {
			return "", fmt.Errorf("workflow: cannot resume: %w", rerr)
		}
		run = existing
		// Inherit the recorded default-resolution (do NOT re-resolve — a mid-run
		// change of default_provider must never re-key an omitted-provider leaf). A
		// PRE-feature manifest carries neither field; resolve+stamp once here so a
		// provider-less script resumed on an old run still has a default.
		if run.DefaultProvider == "" && run.DefaultProviderError == "" {
			run.DefaultProvider, run.DefaultProviderError = resolveRunDefault()
		}
		// One engine per run: refuse to resume a run that still has a LIVE engine — a verifiably-live
		// detached one, or a foreground/unreapable run (EnginePID<=0) still claiming to run. The board
		// Restart path stops the old engine first (so the status is no longer running when it reaches
		// here); a crashed/killed detached run (recorded pid now dead) falls through and resumes as-is.
		// This guards the public `workflow run --resume` entry, which would otherwise launch a second
		// concurrent engine. (Mirrors restart.go's fail-closed liveness check.)
		if run.Status == "running" && (subagent.EngineAlive(run) || run.EnginePID <= 0) {
			return "", fmt.Errorf("workflow: run %s already has a live engine; stop it first", opts.Resume)
		}
		// Replay the run's original launch options on resume so a leaf's content key — and thus
		// its journal-cache validity — doesn't shift. A non-zero/non-empty opts value overrides
		// (e.g. resuming with a larger --budget-usd); a zero/empty one reads as "not set" and
		// inherits the manifest. The one way to override TO uncapped is --no-budget, which arrives
		// as a -1 sentinel: it is non-zero so it skips the inherit, then normalizes to 0 (uncapped)
		// below — so a capped run CAN be resumed uncapped. (Args/persistIO have no such sentinel.)
		if opts.ArgsJSON == "" {
			opts.ArgsJSON = existing.ArgsJSON
		}
		if !opts.NoPersistIO {
			opts.NoPersistIO = existing.NoPersistIO
		}
		if opts.BudgetUSD == 0 {
			opts.BudgetUSD = existing.BudgetUSD
		}
		if opts.BudgetTokens == 0 {
			opts.BudgetTokens = existing.BudgetTokens
		}
		// Normalize the -1 uncap sentinel to 0 AFTER the inherit decision (so -1 never inherits),
		// then re-stamp the budgets onto the manifest so an uncap is durable from resume time — the
		// engine never persists -1, and never re-caps a run the user just uncapped.
		normalizeBudgetSentinels(&opts)
		run.BudgetUSD = opts.BudgetUSD
		run.BudgetTokens = opts.BudgetTokens
		// Restamp liveness + flip to running BEFORE detaching, so a concurrent GC can't
		// prune this (possibly old) run's manifest + journal in the window before the
		// resumed engine writes its first heartbeat (GC recency keys on the later of
		// StartedAt / UpdatedAt). A stamp failure means the manifest is unwritable — the
		// run can't be tracked or protected, so fail the resume rather than launch blind.
		run.Status = "running"
		run.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		run.EnginePID = 0 // clear the prior engine's pid; the new child re-stamps, and WaitEngineStarted waits for exactly that
		if serr := subagent.SaveRun(run); serr != nil {
			return "", fmt.Errorf("workflow: stamp resume liveness: %w", serr)
		}
	} else {
		minted, perr := Prepare(abs)
		if perr != nil {
			return "", perr
		}
		run = minted
		// Stamp the session + replay options onto the manifest BEFORE the child reads it, so
		// the board can group by session and a later restart resumes with the same inputs. A fresh
		// --no-budget is a no-op (already uncapped) but still normalize -1→0 so the manifest never
		// persists the sentinel.
		normalizeBudgetSentinels(&opts)
		run.SessionID = opts.LeadSessionID
		run.ArgsJSON = opts.ArgsJSON
		run.NoPersistIO = opts.NoPersistIO
		run.BudgetUSD = opts.BudgetUSD
		run.BudgetTokens = opts.BudgetTokens
		// Resolve the run's default provider ONCE at mint and record the RESULT (the
		// provider OR the error code a provider-less agent() will throw). An all-explicit
		// script never reads either, so an unresolvable default does not block launch.
		run.DefaultProvider, run.DefaultProviderError = resolveRunDefault()
		if cwd, cerr := os.Getwd(); cerr == nil {
			run.Cwd = cwd
		}
		if serr := subagent.SaveRun(run); serr != nil {
			return "", fmt.Errorf("workflow: persist run options: %w", serr)
		}
	}
	// Persist the script as the run's saved-script sidecar (the saved-script slice of
	// native save-script) so a stopped run can be restarted from the board
	// (`workflow run --resume <id>`). Best-effort: a write hiccup just means restart
	// needs the original path.
	if data, rerr := os.ReadFile(abs); rerr == nil {
		if sp, serr := subagent.RunScriptPath(run.RunID); serr == nil {
			_ = os.WriteFile(sp, data, 0o600)
		}
	}
	if foreground {
		return run.RunID, Execute(ctx, abs, run.RunID, opts)
	}
	pid, reaper, lerr := launchDetached(abs, run.RunID, opts)
	if lerr != nil {
		run.Status = "failed"
		_ = subagent.SaveRun(run)
		return "", lerr
	}
	if !WaitEngineStarted(run.RunID, pid) {
		// The detached child did not self-stamp its pid within the startup budget (slow OR
		// dead). Kill + reap it BEFORE failing the run, so a slow-but-live child can't later
		// overwrite the failed manifest and run on as a second engine.
		reaper.kill()
		_ = reaper.wait()
		run.Status, run.Error, run.EnginePID = "failed", "workflow: engine did not register within the startup budget", 0
		_ = subagent.SaveRun(run)
		return "", fmt.Errorf("workflow: engine failed to start")
	}
	// Registered: reap the child asynchronously so it never lingers as a zombie under a
	// long-lived parent (the TUI board). A short-lived CLI launcher exits before this
	// goroutine completes; init then adopts and reaps the orphan.
	go func() { _ = reaper.wait() }()
	return run.RunID, nil
}

// engineStartupBudget bounds how long a launcher waits — under the per-run execution lock —
// for a freshly detached engine child to self-stamp its pid into the manifest. (A var so a test can
// shorten it; production never reassigns it.)
var engineStartupBudget = 10 * time.Second

// WaitEngineStarted blocks until the detached child has self-stamped EnginePID == childPID
// (its first manifest write in Execute's pre-run saveManifest) or the startup budget
// elapses. The launcher holds the per-run execution lock across this wait, so a serialized
// second restart/stop/resume always observes a fully-registered engine — never the
// pre-stamp EnginePID==0 window. Returns false on timeout; the caller must then kill+reap
// the child before failing the run.
func WaitEngineStarted(runID string, childPID int) bool {
	deadline := time.Now().Add(engineStartupBudget)
	for {
		if run, err := subagent.ReadRun(runID); err == nil && run.EnginePID == childPID {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}
