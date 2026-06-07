package workflow

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// engine holds the per-run state the builtins close over. It is the single
// authoritative writer of the run manifest — name/description/startedAt are fixed at
// Execute, phases + currentPhase accumulate as the script announces them, and every
// phase()/finalize OVERWRITES the whole manifest from this state. All fields are only
// read/written by a builtin (or Execute's finalize), and every builtin runs while its
// goroutine holds the GIL, so they need no separate lock (the GIL serializes access).
type engine struct {
	sched        *scheduler
	runID        string
	name         string
	description  string
	startedAt    string
	phases       []subagent.RunPhase
	currentPhase string
	// journal is the run's content-hash result cache (resume). Nil-safe: an engine
	// built without one (the leaf unit tests) simply never caches. Read/written only
	// under the GIL (lookup before a leaf's exec; append after runBlocking re-locks).
	journal *journal
	// events is the run's live-event channel that `workflow watch` renders. Nil-safe. Emitted
	// only under the GIL — the seq counter needs no atomic and lines never interleave. One-way
	// producer→watcher; never read back by the engine, never feeds journalKey.
	events    *eventWriter
	groupSeq  int    // monotonic id source for parallel/pipeline/workflow group brackets
	persistIO bool   // persist each leaf's prompt+answer for the board's inline detail (default on; --no-persist-io off)
	enginePID int    // os.Getpid() of the DETACHED engine — recorded so `workflow stop` can reap it
	metaModel string // meta.model: default model for agents that omit model= (applied before journalKey)
	whenToUse string // meta.whenToUse: display/board text
	sessionID string // parent Claude session (board grouping); seeded from the manifest, re-persisted every save
	cwd       string // launching project dir (board run header); seeded from the manifest, re-persisted every save
	argsJSON  string // --args-json, re-persisted so a restart resumes with the SAME args (else leaf keys shift)
	// Budget accounting, GIL-protected. A cap (<=0 = uncapped) trips on the FIRST of two
	// counters to breach: USD (an Anthropic list-price ESTIMATE — claude's own metering, not
	// the third-party vendor's actual charge) and tokens (Usage.InputTokens+OutputTokens, the
	// exact vendor-neutral ceiling). *Spent accumulates each completed leaf's real cost; *Reserved
	// holds a pessimistic per-leaf estimate from the budget gate until the leaf reconciles to real,
	// so a concurrent fan-out admits leaves against spent+reserved (not spent alone) and can't
	// overshoot the cap by the whole in-flight set. See budgetReserve/budgetRelease/budgetCharge.
	budgetTotal    float64
	budgetSpent    float64
	budgetReserved float64
	// Token cap twin of the USD fields (int64, exact). budgetTokensTotal<=0 = uncapped.
	budgetTokensTotal    int64
	budgetTokensSpent    int64
	budgetTokensReserved int64
	// Slim version-gate result, resolved ONCE per engine by effProfileFor: fingerprint
	// load + binary detection are too expensive to pay per leaf under the GIL (with slim
	// the default, every bare leaf resolves — per-leaf resolution would serialize the
	// whole fanout), and a single resolution keeps one run's journal keys on ONE
	// effective shape even if the host claude changes mid-run.
	gateOnce   sync.Once
	gateOK     bool
	gateReason string
}

// effProfileFor maps a REQUESTED prompt profile to the effective one, consulting the
// engine's once-resolved slim gate. full/"" pass through without resolving.
func (e *engine) effProfileFor(requested string) (string, string) {
	if requested == "" || requested == subagent.ProfileFull {
		return requested, ""
	}
	e.gateOnce.Do(func() {
		eff, reason := resolveProfile(subagent.ProfileSlim)
		e.gateOK, e.gateReason = eff == subagent.ProfileSlim, reason
	})
	if !e.gateOK {
		return subagent.ProfileFull, e.gateReason
	}
	return requested, ""
}

// builtins returns the predeclared environment exposed to a workflow script. It
// mirrors the native Workflow API; the only shape differences are agent()'s vendor=
// parameter and Starlark syntax. `args` is predeclared when --args-json was given.
func (e *engine) builtins(opts Options) starlark.StringDict {
	env := starlark.StringDict{
		"agent":    starlark.NewBuiltin("agent", e.agent),
		"parallel": starlark.NewBuiltin("parallel", e.parallel),
		"pipeline": starlark.NewBuiltin("pipeline", e.pipeline),
		"phase":    starlark.NewBuiltin("phase", e.phase),
		"log":      starlark.NewBuiltin("log", e.log),
		// `wait` is cc-fleet's name for native's await() — Starlark RESERVES `await` as a
		// keyword, so the script-facing builtin must use a legal identifier.
		"wait":     starlark.NewBuiltin("wait", e.await),
		"workflow": starlark.NewBuiltin("workflow", e.workflow),
		"budget":   budgetValue{e: e},
	}
	if opts.ArgsJSON != "" {
		// Decode --args-json into `args` on the not-yet-concurrent top-level thread.
		// Best-effort: a bad value just leaves `args` absent.
		th := e.sched.newThread("workflow:args")
		if v, err := starlark.Call(th, jsonDecode, starlark.Tuple{starlark.String(opts.ArgsJSON)}, nil); err == nil {
			v.Freeze() // immutable, like a frozen global — script-side mutation is a clean error
			env["args"] = v
		}
	}
	return env
}

// agent runs ONE vendor subagent leaf and blocks the calling Starlark thread until
// it returns. On a leaf failure it RAISES a Starlark error (faithful to native: a
// bare top-level agent() aborts the run; parallel/pipeline recover the error into a
// None at that index). With schema= the leaf runs with --json-schema (claude injects
// and enforces a forced StructuredOutput tool call); the returned payload is
// validated against the schema as a client backstop — an absent or invalid payload
// fails the leaf (no retry). The prompt is fed via stdin (PromptReader), never argv.
// Entered + returns with the GIL held; the GIL is released only for the blocking
// exec + slot wait (via runBlocking).
func (e *engine) agent(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		prompt       string
		vendor       string
		modelVal     starlark.Value
		schemaVal    starlark.Value
		labelVal     starlark.Value
		phaseVal     starlark.Value
		timeoutVal   starlark.Value
		budgetVal    starlark.Value
		turnsVal     starlark.Value
		bgVal        starlark.Value
		isolationVal starlark.Value
		profileVal   starlark.Value
		toolsVal     starlark.Value
		skillsVal    starlark.Value
		mcpVal       starlark.Value
	)
	// Every optional is unpacked as a Value so an explicit None (the documented
	// "omitted" default) is accepted rather than rejected by Starlark's strict typing;
	// the opt* helpers coerce None → zero. timeout/max_budget_usd also accept int OR
	// float so a script can write the natural timeout=120 / max_budget_usd=1.
	if err := starlark.UnpackArgs(b.Name(), args, kwargs,
		"prompt", &prompt,
		"vendor", &vendor,
		"model?", &modelVal,
		"schema?", &schemaVal,
		"label?", &labelVal,
		"phase?", &phaseVal,
		"timeout?", &timeoutVal,
		"max_budget_usd?", &budgetVal,
		"max_turns?", &turnsVal,
		"run_in_background?", &bgVal,
		"isolation?", &isolationVal,
		"profile?", &profileVal,
		"tools?", &toolsVal,
		"skills?", &skillsVal,
		"mcp?", &mcpVal,
	); err != nil {
		return nil, err
	}
	if vendor == "" {
		return nil, fmt.Errorf("agent: vendor= is required")
	}
	runInBackground := bgVal != nil && bgVal != starlark.None && bool(bgVal.Truth())
	isolation, err := optString(isolationVal, "isolation")
	if err != nil {
		return nil, err
	}
	if isolation != "" && isolation != "worktree" {
		return nil, fmt.Errorf("agent: isolation must be 'worktree' (or omitted), got %q", isolation)
	}
	if isolation == "worktree" && runInBackground {
		return nil, fmt.Errorf("agent: isolation='worktree' is not supported with run_in_background=True")
	}
	model, err := optString(modelVal, "model")
	if err != nil {
		return nil, err
	}
	label, err := optString(labelVal, "label")
	if err != nil {
		return nil, err
	}
	phaseArg, err := optString(phaseVal, "phase")
	if err != nil {
		return nil, err
	}
	timeoutSec, err := optFloat(timeoutVal, "timeout")
	if err != nil {
		return nil, err
	}
	maxBudget, err := optFloat(budgetVal, "max_budget_usd")
	if err != nil {
		return nil, err
	}
	maxTurns, err := optInt(turnsVal, "max_turns")
	if err != nil {
		return nil, err
	}
	profile, err := optStringDefault(profileVal, "profile", subagent.ProfileSlim)
	if err != nil {
		return nil, err
	}
	tools, err := optStringList(toolsVal, "tools")
	if err != nil {
		return nil, err
	}
	skills, err := optBoolDefault(skillsVal, "skills", true)
	if err != nil {
		return nil, err
	}
	mcp, mcpPresent, err := optBool(mcpVal, "mcp")
	if err != nil {
		return nil, err
	}
	// Front-load the same slim validation the bare-CLI path uses, surfaced as Starlark
	// errors (consistent with the other kwarg errors above): the profile enum, the
	// slim-only refinements rejected when combined with full, and tool canonicalization.
	if perr := subagent.ValidateProfile(profile); perr != nil {
		return nil, fmt.Errorf("agent: %w", perr)
	}
	isFull := profile == "" || profile == subagent.ProfileFull
	if isFull && (len(tools) > 0 || !skills || mcpPresent) {
		return nil, fmt.Errorf("agent: tools= / skills= / mcp= are slim-only; they require profile='slim' or 'slim-ro'")
	}
	canonTools, err := canonicalizeTools(tools)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if err := subagent.ValidateToolsSkills(canonTools, !skills); err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}

	// CONVERT-UNDER-LOCK: snapshot every Starlark input into Go data while the GIL is
	// held, before releasing it for the blocking exec.
	phaseTag := phaseArg
	if phaseTag == "" {
		phaseTag = e.currentPhase
	}
	// meta.model is the default model for agents that omit model=. Apply it BEFORE the
	// journal key so the key reflects the EFFECTIVE model the leaf will use.
	if model == "" {
		model = e.metaModel
	}
	// Resolve the EFFECTIVE profile (post-version-gate) BEFORE keying, same as meta.model:
	// a below-floor/unknown claude downgrades a slim request to full, and the key must fold
	// the effective shape so a cross-machine resume can't replay a full answer under a slim
	// key. The gate is resolved once per engine (effProfileFor).
	effProfile, downgrade := e.effProfileFor(profile)
	// Resolve the EFFECTIVE tool set against the effective profile BEFORE keying: an
	// explicit tools= when given, else the profile default (DefaultSlimTools, canonicalized).
	// Folding the resolved set — and passing the SAME set to the leaf via Request.Tools —
	// keeps keying and execution from diverging (a bare slim leaf keys with nil tools while
	// running DefaultSlimTools otherwise). For a full effective profile the slim fields don't
	// fold and Run ignores Tools, so the resolved set is inert there.
	keyTools := canonTools
	if (effProfile == subagent.ProfileSlim || effProfile == subagent.ProfileSlimRO) && len(keyTools) == 0 {
		keyTools, err = subagent.CanonicalizeTools(subagent.DefaultSlimTools(effProfile, !skills))
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
	}
	// MCP per-profile default, resolved in the same pre-keying window as the tool
	// set: an explicit mcp= wins; else slim inherits the host config (native
	// generic parity) and slim-ro stays strict. Inert for an effective-full
	// profile (not folded into the key, not emitted in the argv).
	if !mcpPresent {
		mcp = effProfile == subagent.ProfileSlim
	}
	var schemaJSON string
	if schemaVal != nil && schemaVal != starlark.None {
		sj, serr := encodeSchema(thread, schemaVal)
		if serr != nil {
			return nil, fmt.Errorf("agent: schema: %w", serr)
		}
		schemaJSON = sj
	}
	if runInBackground && schemaJSON != "" {
		// A background leaf's result is read back from its job file at await time, and
		// the structured payload is in-process only — so schema= isn't offered for it.
		return nil, fmt.Errorf("agent: schema= is not supported with run_in_background=True")
	}

	// Log the version-gate downgrade BEFORE the journal lookup, so it is visible even when
	// a cache hit returns without executing (the only place the leaf shape would otherwise
	// be invisible). Routed through log() — the engine's user-visible narrator line.
	if downgrade != "" {
		e.logf("agent(%s): %s; running full", vendor, downgrade)
	}

	// Resume replay: a journaled leaf returns its cached result with NO vendor exec, NO
	// slot, and NO lifetime-admit — a cache hit is free. The key spans the result's full
	// determinant (vendor / model / base prompt / schema / effective slim shape). A schema
	// leaf re-decodes + re-validates the cached raw answer (deterministic: it passed before).
	key := journalKey(vendor, model, prompt, schemaJSON, isolation, effProfile, keyTools, !skills, mcp)
	if cached, ok := e.journal.lookup(key); ok {
		if runInBackground {
			// A resolved handle: await() returns the cached result without spawning.
			return &bgHandle{resolved: true, cached: cached, key: key, vendor: vendor, model: model, phase: phaseTag, label: label}, nil
		}
		if schemaJSON == "" {
			e.emitLeaf("cached", phaseTag, label, vendor, model)
			return starlark.String(cached), nil
		}
		// A schema leaf re-decodes + re-validates its cached raw answer (deterministic:
		// it passed before). If a corrupt/hand-edited journal entry fails to validate,
		// fall through to re-run the leaf rather than abort the run.
		if v, verr := decodeAndValidate(thread, cached, schemaVal); verr == nil {
			e.emitLeaf("cached", phaseTag, label, vendor, model)
			return v, nil
		}
	}

	// Budget gate: a real exec is about to spend, so refuse once a cap would be breached.
	// Placed AFTER the journal lookup (a cache hit is free and never blocked) and BEFORE the
	// bg branch + admit/slot. A leaf RESERVES a pessimistic per-leaf estimate against each cap
	// so a concurrent fan-out admits against spent+reserved — not spent alone — and can't
	// overshoot the cap by the whole in-flight set; the estimate reconciles to real on completion.
	// The USD estimate over-counts a typical leaf (its own max_budget_usd wins when larger); the
	// token estimate is the flat per-leaf floor. The gate→reserve is GIL-held (atomic), so a
	// parallel fan-out's leaves serialize through it. First cap to trip aborts.
	usdEst := defaultLeafEstimate
	if maxBudget > usdEst {
		usdEst = maxBudget
	}
	tokEst := int64(defaultLeafTokenEstimate)
	if e.budgetWouldExceed(usdEst, tokEst) {
		return nil, e.budgetExceededErr()
	}

	// Background leaf: launch detached, return a handle (NO live-pool slot; result is journaled at
	// await time). launchBg does its own lifetime admit AND owns the reservation lifecycle — it
	// reserves before the launch and carries the estimate on the handle so resolveHandle releases
	// (and charges real) at await; a launch failure releases there.
	if runInBackground {
		return e.launchBg(vendor, model, prompt, phaseTag, label, key, timeoutSec, maxBudget, maxTurns,
			usdEst, tokEst, slimReq{profile: profile, tools: keyTools, noSkills: !skills, mcp: mcp})
	}

	if !e.sched.admit() {
		return nil, fmt.Errorf("agent: run exceeded the %d-leaf lifetime cap", maxLifetimeAgents)
	}

	// Reserve this sync leaf's estimate (GIL-held, atomic with the gate above), then register the
	// release defer — placed AFTER the reservation so it never over-releases a cache-hit/bg return,
	// and it runs GIL-held on the unwind (like the slot defer below), covering EVERY exit beyond
	// here (slot-fail, worktree-fail, leaf-fail, schema-invalid, panic, success). Success charges
	// real BEFORE returning, so the defer then frees only the estimate.
	e.budgetReserve(usdEst, tokEst)
	defer e.budgetRelease(usdEst, tokEst)

	// Mint a queued placeholder (PID=0) so the board shows this leaf as a queued ◌ row WHILE it
	// waits for a pool slot; subagent.Run reuses this id, flipping the same job queued→running→
	// terminal as one file. The mint writes a file, so it runs GIL-released (like the slot wait).
	var queuedJobID string
	e.sched.runBlocking(func() {
		queuedJobID = mintQueuedLeaf(subagent.Request{
			Vendor: vendor, RunID: e.runID, Phase: phaseTag, Label: label,
			JournalKey: key, PersistIO: e.persistIO, PromptProfile: profile,
		}, model)
	})
	// Guarantee the reused job ends terminal: unless a success return sets leafDone, this defer
	// finalizes it FAILED on EVERY other exit (slot-cancel, worktree-fail, pre-flight vendor fail,
	// schema-invalid, panic) — so a queued placeholder never lingers and a schema-invalid "done"
	// attempt is corrected. leafErr carries a real failure's error class (preserved); else canonical.
	leafDone := false
	var leafErr subagent.Result
	defer func() {
		if !leafDone {
			subagent.FinalizeQueuedLeafFailed(queuedJobID, leafErr)
		}
	}()

	// Acquire a pool slot with the GIL released, so waiting for a slot never pins the
	// interpreter. The slot is held ONLY across this leaf's actual exec and released right
	// after — never across nested parallel/pipeline/workflow inside an element — so nesting
	// can't deadlock on a slot a parent branch is sitting on. Only register the release
	// defer AFTER a successful acquire.
	acquired := false
	e.sched.runBlocking(func() { acquired = e.sched.acquireSlot() })
	if !acquired {
		return nil, fmt.Errorf("agent: run cancelled before launch")
	}
	defer e.sched.releaseSlot()

	// Worktree isolation: run the leaf with cwd = a fresh git worktree, torn down on
	// return (success, failure, or panic).
	// Created with the GIL RELEASED (it shells out to git); GIL re-held on return.
	var workDir string
	if isolation == "worktree" {
		var cleanup func()
		var werr error
		e.sched.runBlocking(func() { workDir, cleanup, werr = createWorktreeFn(e.runID) })
		if werr != nil {
			e.emitLeaf("failed", phaseTag, label, vendor, model)
			return nil, fmt.Errorf("agent: %w", werr)
		}
		defer cleanup()
	}

	e.emitLeaf("launch", phaseTag, label, vendor, model)
	req := subagent.Request{
		Vendor:         vendor,
		Model:          model,
		PromptReader:   strings.NewReader(prompt), // stdin, not argv
		JSON:           true,                      // force inner json → res.Result is the answer text
		Timeout:        time.Duration(timeoutSec * float64(time.Second)),
		MaxTurns:       maxTurns,
		MaxBudgetUSD:   maxBudget,
		RunID:          e.runID,
		Phase:          phaseTag,
		Label:          label,
		JobID:          queuedJobID, // reuse the queued placeholder: one job, queued→running→terminal
		Attempt:        1,           // single exec — a schema mismatch is terminal, never a retry
		JournalKey:     key,         // persisted so the board can restart THIS leaf (invalidate + resume)
		PersistIO:      e.persistIO,
		StreamActivity: e.persistIO, // sync leaf streams tool/usage activity for the board (gated like PersistIO)
		IOPrompt:       prompt,      // persisted only when PersistIO (subagent gates)
		WorkingDir:     workDir,     // empty unless isolation='worktree'
		// Slim profile: Run re-resolves the EFFECTIVE profile (its own version gate); the
		// REQUESTED profile is passed here, the engine's effProfile only keys + logs above.
		PromptProfile: profile,
		Tools:         keyTools, // the resolved key set — the leaf execs exactly what was keyed
		NoSkills:      !skills,
		MCP:           mcp,
		JSONSchema:    schemaJSON,
	}
	var res subagent.Result
	e.sched.runBlocking(func() { res = runLeaf(req) })
	if !res.OK {
		leafErr = res // a pre-flight fail (no Run registration) keeps its real error class on the job
		e.emitLeaf("failed", phaseTag, label, vendor, model)
		return nil, fmt.Errorf("agent(%s): %s: %s", vendor, res.ErrorCode, res.ErrorMsg)
	}
	// Book the exec's real cost under the GIL: USD (claude's list-price estimate) +
	// tokens (input+output). The reservation is freed by the defer at return; charging
	// before it keeps spent+reserved monotonic for concurrent gates.
	e.budgetCharge(res.CostUSD, leafTokens(res))
	if schemaJSON == "" {
		leafDone = true // subagent.Run finalized this job done; keep it
		e.journal.append(key, res.Result)
		e.emitLeaf("done", phaseTag, label, vendor, model)
		return starlark.String(res.Result), nil
	}
	// Schema leaf: claude enforced that the StructuredOutput tool was CALLED; the
	// client validation below is the backstop for a weak vendor filling it
	// invalidly. An OK envelope WITHOUT the payload (e.g. a max_turns-starved
	// leaf) is a failure — never a prose-JSON fallback. Validation failure is
	// terminal: an identical re-run reproduces it at full leaf cost. leafDone stays
	// false on both paths, so the deferred finalize corrects the job Run marked done.
	if len(res.StructuredOutput) == 0 {
		e.emitLeaf("failed", phaseTag, label, vendor, model)
		return nil, fmt.Errorf("agent(%s): schema: no structured_output in the result envelope (the StructuredOutput call costs turns — raise max_turns)", vendor)
	}
	v, verr := decodeAndValidate(thread, string(res.StructuredOutput), schemaVal)
	if verr != nil {
		e.emitLeaf("failed", phaseTag, label, vendor, model)
		return nil, fmt.Errorf("agent(%s): schema not satisfied: %v", vendor, verr)
	}
	leafDone = true
	e.journal.append(key, string(res.StructuredOutput))
	e.emitLeaf("done", phaseTag, label, vendor, model)
	return v, nil
}

// optString coerces an optional string kwarg to a Go string; omitted (nil) or an
// explicit None is "". Accepting None lets a script copy the documented signature
// (model=None, label=None, …) verbatim instead of tripping Starlark's strict typing.
func optString(v starlark.Value, name string) (string, error) {
	if v == nil || v == starlark.None {
		return "", nil
	}
	s, ok := starlark.AsString(v)
	if !ok {
		return "", fmt.Errorf("agent: %s must be a string, got %s", name, v.Type())
	}
	return s, nil
}

// optFloat coerces an optional numeric kwarg (Int or Float) to a float64; omitted (nil)
// or None is 0. Lets a script write the natural timeout=120 / max_budget_usd=1 (ints) as
// well as floats or None, despite Starlark's strict UnpackArgs typing.
func optFloat(v starlark.Value, name string) (float64, error) {
	if v == nil || v == starlark.None {
		return 0, nil
	}
	f, ok := starlark.AsFloat(v)
	if !ok {
		return 0, fmt.Errorf("agent: %s must be a number, got %s", name, v.Type())
	}
	return f, nil
}

// optInt coerces an optional integer kwarg to an int; omitted (nil) or None is 0.
func optInt(v starlark.Value, name string) (int, error) {
	if v == nil || v == starlark.None {
		return 0, nil
	}
	i, ierr := starlark.AsInt32(v)
	if ierr != nil {
		return 0, fmt.Errorf("agent: %s must be an integer, got %s", name, v.Type())
	}
	return i, nil
}

// optStringDefault coerces an optional string kwarg, falling back to def when omitted
// (nil) or None — so a non-empty documented default (profile='slim') is applied without
// a separate empty check.
func optStringDefault(v starlark.Value, name, def string) (string, error) {
	if v == nil || v == starlark.None {
		return def, nil
	}
	s, ok := starlark.AsString(v)
	if !ok {
		return "", fmt.Errorf("agent: %s must be a string, got %s", name, v.Type())
	}
	return s, nil
}

// optBoolDefault coerces an optional bool kwarg, falling back to def when omitted (nil)
// or None — so skills= (default true) reads its documented default when the script
// omits it.
func optBoolDefault(v starlark.Value, name string, def bool) (bool, error) {
	if v == nil || v == starlark.None {
		return def, nil
	}
	b, ok := v.(starlark.Bool)
	if !ok {
		return false, fmt.Errorf("agent: %s must be a bool, got %s", name, v.Type())
	}
	return bool(b), nil
}

// optBool coerces an explicit bool kwarg, reporting presence: omitted (nil) or
// None is (false, false) — so mcp= reads its per-profile default only when the
// script truly didn't choose.
func optBool(v starlark.Value, name string) (val, present bool, err error) {
	if v == nil || v == starlark.None {
		return false, false, nil
	}
	b, ok := v.(starlark.Bool)
	if !ok {
		return false, false, fmt.Errorf("agent: %s must be a bool, got %s", name, v.Type())
	}
	return bool(b), true, nil
}

// optStringList coerces an optional list-of-string kwarg to a Go slice; omitted (nil) or
// None is nil. Every element must be a string.
func optStringList(v starlark.Value, name string) ([]string, error) {
	if v == nil || v == starlark.None {
		return nil, nil
	}
	lst, ok := v.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("agent: %s must be a list of strings, got %s", name, v.Type())
	}
	out := make([]string, 0, lst.Len())
	for i := 0; i < lst.Len(); i++ {
		s, ok := starlark.AsString(lst.Index(i))
		if !ok {
			return nil, fmt.Errorf("agent: %s[%d] must be a string, got %s", name, i, lst.Index(i).Type())
		}
		out = append(out, s)
	}
	return out, nil
}

// canonicalizeTools validates + canonicalizes an explicit tools= set (dedupe + sort) so
// caller order never changes the journal key; an empty set stays nil (the profile default
// applies in subagent.Run). Delegates to the single canonical validator so the engine and
// bare-CLI paths reject identically.
func canonicalizeTools(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return subagent.CanonicalizeTools(names)
}

// parallel runs every thunk concurrently, one goroutine + fresh thread each, and
// returns a list of results (None where a thunk raised or panicked). It is a BARRIER
// — it blocks until all thunks finish.
func (e *engine) parallel(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var list starlark.Value
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &list); err != nil {
		return nil, err
	}
	thunks, err := frozenSlice(list, b.Name())
	if err != nil {
		return nil, err
	}
	// Bracket the fan-out with group-open/close events (GIL-held, before/after fanout) so
	// `workflow watch` shows it as a bracketed group (▸ open … ◂ close) in the live stream —
	// every leaf event lands between this group's open and close, without threading a group id
	// through the hot fanout path.
	gid := e.emitGroupOpen("parallel")
	results := make([]starlark.Value, len(thunks))
	e.fanout(thread, len(thunks), func(i int, th *starlark.Thread) {
		results[i] = e.callOrNone(th, thunks[i], nil)
	})
	e.emitGroupClose(gid)
	return starlark.NewList(results), nil
}

// pipeline runs each item through all stages independently with NO inter-stage
// barrier (item A can be in stage 3 while B is in stage 1). Each stage is called
// stage(prev, item, index); a stage error/panic drops that item to None and skips
// its remaining stages. Returns the per-item final results.
func (e *engine) pipeline(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(kwargs) > 0 {
		return nil, fmt.Errorf("pipeline: takes no keyword arguments")
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("pipeline: needs items and at least one stage")
	}
	items, err := frozenSlice(args[0], b.Name())
	if err != nil {
		return nil, err
	}
	stages := make([]starlark.Value, 0, len(args)-1)
	for _, s := range args[1:] {
		s.Freeze()
		stages = append(stages, s)
	}
	gid := e.emitGroupOpen("pipeline")
	results := make([]starlark.Value, len(items))
	e.fanout(thread, len(items), func(i int, th *starlark.Thread) {
		results[i] = e.runPipelineItem(th, items[i], i, stages)
	})
	e.emitGroupClose(gid)
	return starlark.NewList(results), nil
}

// phase sets the run's current phase (used to tag agents that don't pass phase=) and
// records the title on the manifest in first-seen order (live board ordering).
// Best-effort: a manifest hiccup never fails the run. GIL-held, so the in-memory
// phase update and the full manifest overwrite are serialized.
func (e *engine) phase(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var title, detail string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "title", &title, "detail?", &detail); err != nil {
		return nil, err
	}
	e.currentPhase = title
	// Dedup-append against the full in-memory phase set (titles declared in static
	// `meta` AND prior phase() calls) so the manifest never carries a duplicate title
	// (the board groups phases by title into one row); then overwrite the manifest.
	found := false
	for _, p := range e.phases {
		if p.Title == title {
			found = true
			break
		}
	}
	if !found {
		e.phases = append(e.phases, subagent.RunPhase{Title: title, Detail: detail})
	}
	e.saveManifest("running", "")
	e.events.emit(EventRecord{Kind: "phase", Phase: title, Msg: detail})
	return starlark.None, nil
}

// emitLeaf records a leaf transition (launch/done/failed/cached) on the live-event
// channel. GIL-held callers only; nil-safe via the writer.
func (e *engine) emitLeaf(status, phase, label, vendor, model string) {
	e.events.emit(EventRecord{Kind: "leaf", Status: status, Phase: phase, Label: label, Vendor: vendor, Model: model})
}

// logf is the engine-internal narrator: a formatted line to stderr plus a `log`
// live-event, the same surface the log() builtin exposes to scripts. Used for the slim
// version-gate downgrade notice. GIL-held callers only.
func (e *engine) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, "[workflow] "+msg)
	e.events.emit(EventRecord{Kind: "log", Msg: msg})
}

// emitGroupOpen records the start of a parallel/pipeline/workflow group and returns its
// id; emitGroupClose records its end. `workflow watch` brackets the group in its live stream
// by seq order: every event between an open and its matching close belongs to that group, and
// nested groups (e.g. a parallel inside a pipeline stage) bracket by order — so no group id
// has to be threaded through fanout/callOrNone. GIL-held callers only (open before
// fanout, close after it joins).
func (e *engine) emitGroupOpen(groupType string) string {
	e.groupSeq++
	gid := fmt.Sprintf("g%d", e.groupSeq)
	e.events.emit(EventRecord{Kind: "group-open", GroupID: gid, GroupTy: groupType, Phase: e.currentPhase})
	return gid
}

func (e *engine) emitGroupClose(gid string) {
	e.events.emit(EventRecord{Kind: "group-close", GroupID: gid})
}

// saveManifest overwrites the run manifest from the engine's authoritative in-memory
// state (errText is recorded only on a failed finalize). Best-effort: a write hiccup
// never fails the run. It is called by phase() under the GIL during the run, and by
// Execute's pre-run stamp + deferred finalize when no leaf goroutine is live — so
// manifest writes never race.
func (e *engine) saveManifest(status, errText string) {
	_ = subagent.SaveRun(subagent.WorkflowRun{
		RunID:       e.runID,
		Name:        e.name,
		Description: e.description,
		WhenToUse:   e.whenToUse,
		StartedAt:   e.startedAt,
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339),
		Phases:      e.phases,
		Status:      status,
		Error:       errText,
		EnginePID:   e.enginePID,
		// Carried in engine state so every whole-file overwrite preserves them (the board
		// groups by SessionID; a restart resumes with the same args/persistIO/budget).
		SessionID:    e.sessionID,
		Cwd:          e.cwd,
		ArgsJSON:     e.argsJSON,
		NoPersistIO:  !e.persistIO,
		BudgetUSD:    e.budgetTotal,
		BudgetTokens: e.budgetTokensTotal,
		SpentUSD:     e.budgetSpent,
		SpentTokens:  e.budgetTokensSpent,
	})
}

// log writes a narrator line to stderr (diagnostic — discarded when the run is detached,
// visible with --foreground) AND emits a live-event record that `workflow watch` renders.
// stdout stays clean for the run id the launcher prints; the stderr stream itself is not
// persisted.
func (e *engine) log(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var msg string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "msg", &msg); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "[workflow] "+msg)
	e.events.emit(EventRecord{Kind: "log", Msg: msg})
	return starlark.None, nil
}

// fanout releases the GIL, runs work(i, thread) on a fresh goroutine + thread for each
// i in [0,n), waits for all, then re-acquires the GIL. work runs with the GIL HELD
// (each goroutine acquires it for its starlark.Call) and stores its own result. The
// GIL is released for the whole wait so the goroutines can acquire it; on return it is
// re-acquired so the interpreter resumes single-threaded.
func (e *engine) fanout(parent *starlark.Thread, n int, work func(i int, th *starlark.Thread)) {
	// Propagate the nested-workflow marker to the goroutine threads so a workflow() call
	// from a NESTED run's parallel/pipeline branch is still caught by the one-level guard.
	nested := parent != nil && parent.Local(nestedLocalKey) != nil
	// Each element gets its own goroutine; the pool SLOT is taken inside agent() for the
	// actual (uncached) leaf exec and released right after — NOT held across the element's
	// whole branch. That is what makes nesting deadlock-free: a parallel/pipeline/workflow
	// INSIDE an element doesn't sit on a slot while its own leaves wait for one. Concurrent
	// vendor execs are still pool-bounded (the slot); a large list just queues as cheap
	// slot-blocked goroutines (frozenSlice's maxFanoutElements bounds the list). A
	// branch-held permit (true acquire-then-go) was rejected: it deadlocks when both
	// branches of a parallel nest another parallel that needs a slot the parents hold.
	var wg sync.WaitGroup
	e.sched.unlock()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			th := e.sched.newThread(fmt.Sprintf("workflow:%s:%d", e.runID, i))
			if nested {
				th.SetLocal(nestedLocalKey, true)
			}
			work(i, th)
		}(i)
	}
	wg.Wait()
	e.sched.lock()
}

// callOrNone calls fn(args...) under the GIL, returning its result, or None on a
// Starlark error OR a recovered Go panic (faithful: a failed parallel/pipeline branch
// degrades to None). It locks the GIL for the Call and unlocks in a defer; because
// runBlocking keeps the GIL held across any panic-unwind, that single unlock is always
// correct.
func (e *engine) callOrNone(th *starlark.Thread, fn starlark.Value, args starlark.Tuple) (out starlark.Value) {
	out = starlark.None
	e.sched.lock()
	defer func() {
		_ = recover() // a panicking leaf/thunk → None, run survives
		e.sched.unlock()
	}()
	if v, err := starlark.Call(th, fn, args, nil); err == nil {
		out = v
	}
	return out
}

// runPipelineItem threads one item through the stages, GIL-held, stopping at the first
// stage that errors/panics (→ None). Mirrors callOrNone's GIL + recover discipline.
func (e *engine) runPipelineItem(th *starlark.Thread, item starlark.Value, index int, stages []starlark.Value) (out starlark.Value) {
	out = starlark.None
	e.sched.lock()
	defer func() {
		_ = recover()
		e.sched.unlock()
	}()
	prev := item
	idx := starlark.MakeInt(index)
	for _, stage := range stages {
		v, err := starlark.Call(th, stage, starlark.Tuple{prev, item, idx}, nil)
		if err != nil {
			return starlark.None
		}
		prev = v
	}
	out = prev
	return out
}

// frozenSlice snapshots an iterable into a Go slice AND freezes each element, under
// the GIL. Two distinct guarantees: the GIL is what makes execution -race clean (one
// goroutine in the interpreter at a time, even for unfrozen shared globals); freezing
// each snapshotted thunk/item adds DETERMINISM — a thunk that (transitively, via a
// captured cell) mutates shared state fails deterministically to a "cannot mutate
// frozen" error → None, rather than producing an interleaved result. The element count
// is capped at maxFanoutElements only to bound the results slice against a pathological
// list; concurrent vendor EXECS are bounded by the pool slot (held only inside agent()
// across a leaf's exec), and the per-run lifetime cap is the real ceiling on leaf execs.
func frozenSlice(v starlark.Value, fname string) ([]starlark.Value, error) {
	it, ok := v.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("%s: expected an iterable, got %s", fname, v.Type())
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []starlark.Value
	var x starlark.Value
	for iter.Next(&x) {
		if len(out) >= maxFanoutElements {
			return nil, fmt.Errorf("%s: more than %d elements — split the work into smaller batches", fname, maxFanoutElements)
		}
		x.Freeze()
		out = append(out, x)
	}
	return out, nil
}
