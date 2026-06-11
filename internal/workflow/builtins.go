package workflow

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// ensureLeafProxy ensures the codex conversion daemon for a codex provider before a
// leaf executes (a no-op for every other provider). A seam so tests never start a daemon.
var ensureLeafProxy = codexproxy.EnsureForProviderName

// engine holds the per-run state the builtins close over. It is the single
// authoritative writer of the run manifest — name/description/startedAt are fixed at
// Execute, phases + currentPhase accumulate as the script announces them, and every
// phase()/finalize OVERWRITES the whole manifest from this state. Every field is
// read/written only on the engine loop (loop.go) — the one goroutine that runs JS and
// the state callbacks — so none of it needs a lock; leaf goroutines see only a
// plain-data leafSpec and the post() channel.
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
	// on the loop (lookup in agent(), append in a completion's state half).
	journal *journal
	// events is the run's live-event channel that `workflow watch` renders. Nil-safe.
	// Emitted only on the loop — the seq counter needs no atomic and lines never
	// interleave. One-way producer→watcher; never read back, never feeds journalKey.
	events          *eventWriter
	groupSeq        int    // monotonic id source for parallel/pipeline/workflow group brackets
	persistIO       bool   // persist each leaf's prompt+answer for the board's inline detail (default on; --no-persist-io off)
	enginePID       int    // os.Getpid() of the DETACHED engine — recorded so `workflow stop` can reap it
	engineProcStart string // enginePID's start token — the stop identity where argv is unreadable (Windows)
	metaModel       string // meta.model: default model for agents that omit model (applied before journalKey)
	whenToUse       string // meta.whenToUse: display/board text
	sessionID       string // parent Claude session (board grouping); seeded from the manifest, re-persisted every save
	cwd             string // launching project dir (board run header); seeded from the manifest, re-persisted every save
	argsJSON        string // --args-json, re-persisted so a restart resumes with the SAME args (else leaf keys shift)
	// The run's default-provider resolution, fixed at mint and seeded from the manifest
	// (re-persisted every save so the whole-manifest overwrite can't wipe it). A
	// provider-less agent() resolves to defaultProvider; when it is empty, agent() throws
	// defaultProviderErr (the recorded error_code). One of the two is set when the run
	// uses a default; both empty for an all-explicit script.
	defaultProvider    string
	defaultProviderErr string
	// Budget accounting, loop-protected. A cap (<=0 = uncapped) trips on the FIRST of two
	// counters to breach: USD (an Anthropic list-price ESTIMATE — claude's own metering, not
	// the third-party provider's actual charge) and tokens (Usage.InputTokens+OutputTokens, the
	// exact provider-neutral ceiling). *Spent accumulates each completed leaf's real cost; *Reserved
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
	// load + binary detection are too expensive to pay per leaf (with slim the default,
	// every bare leaf resolves), and a single resolution keeps one run's journal keys on
	// ONE effective shape even if the host claude changes mid-run.
	gateOnce   sync.Once
	gateOK     bool
	gateReason string

	// The loop machinery (loop.go): vm + cbs + loopDone are shared with leaf goroutines
	// (the channel/done handoff only); everything else is loop-owned.
	vm            *goja.Runtime
	cbs           chan leafCB
	loopDone      chan struct{}
	inflight      int  // leaves spawned but not yet completed
	aborted       bool // drain state-only; no more JS runs
	stopped       bool // the run ctx was cancelled (finalizes "stopped", not "failed")
	runCtx        context.Context
	leafCtx       context.Context // child of runCtx; cancelled on abort → leaf execs die promptly
	cancelLeaves  context.CancelFunc
	unhandled     map[*goja.Promise]goja.Value // rejected-with-no-handler set (handled-set semantics)
	ctl           map[string]*leafCtl          // jobID → directive state (loop-owned; the control plane's target map)
	heldPhases    map[string]bool              // phase titles under a stop-phase directive (new members park)
	jsonParse     goja.Callable
	jsonStringify goja.Callable
	errCtor       *goja.Object
	nestedStub    goja.Value // the child scripts' `workflow` parameter: throws the one-level guard
	workflowFn    goja.Value // the top script's `workflow` parameter: jsWorkflow
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

// bootstrapJS runs once at VM setup, before any user statement: it installs the
// deterministic lockdown (the wall-clock Date entry points and Math.random throw; the
// Function-constructor escape on the three function prototypes is sealed — deleting the
// Function global alone leaves `(function(){}).constructor("…")` compiling code) and
// defines parallel/pipeline in JS over the host bracket hooks — Promise composition is
// the natural expression of their barrier / no-barrier contracts — and aliases console
// onto the log() narrator (provider-model muscle memory writes console.log). The host
// object is deleted right after; user scripts can't reach it.
const bootstrapJS = `(function (host) {
	"use strict";
	const unavailable = (name) => () => {
		throw new Error(name + " is unavailable: workflow scripts are deterministic — pass timestamps or randomness in via args");
	};
	// Date keeps its deterministic entry points (an explicit-value construction,
	// parse, UTC) and throws only where the wall clock would leak in.
	const RealDate = Date;
	const NoClock = function (...a) {
		if (a.length === 0) {
			unavailable("new Date() with no arguments")();
		}
		return new RealDate(...a);
	};
	NoClock.prototype = RealDate.prototype;
	NoClock.parse = RealDate.parse;
	NoClock.UTC = RealDate.UTC;
	NoClock.now = unavailable("Date.now()");
	globalThis.Date = NoClock;
	Math.random = unavailable("Math.random()");
	const seal = (proto) => Object.defineProperty(proto, "constructor", { value: undefined, writable: false, configurable: false });
	seal(Object.getPrototypeOf(function () {}));
	seal(Object.getPrototypeOf(async function () {}));
	seal(Object.getPrototypeOf(function* () {}));
	// console maps onto log(): strings pass through, Errors render by message,
	// everything else as JSON via the stringify captured here (immune to script
	// shadowing). show() always returns a string — a value that defeats both JSON
	// and string coercion (e.g. a circular null-prototype object) must not turn
	// an observability call into a run failure.
	const hostLog = globalThis.log;
	const jsonShow = JSON.stringify;
	const NativeError = Error;
	const show = (v) => {
		try {
			if (typeof v === "string") return v;
			if (v instanceof NativeError) return String(v);
			const s = jsonShow(v);
			if (typeof s === "string") return s;
			return String(v);
		} catch (e) {
			try { return String(v); } catch (e2) { return "[unprintable]"; }
		}
	};
	const clog = (...a) => { hostLog(a.map(show).join(" ")); };
	globalThis.console = { log: clog, info: clog, warn: clog, error: clog, debug: clog };
	globalThis.parallel = (thunks) => {
		if (!Array.isArray(thunks)) throw new TypeError("parallel: expected an array of functions (thunks)");
		if (thunks.length > host.maxFanout) throw new RangeError("parallel: more than " + host.maxFanout + " elements - split the work into smaller batches");
		const gid = host.groupOpen("parallel");
		const one = (t) => {
			try {
				if (typeof t !== "function") throw new TypeError("parallel: element is not a function");
				return Promise.resolve(t()).catch(() => null);
			} catch (e) {
				return Promise.resolve(null);
			}
		};
		return Promise.all(thunks.map(one)).then((res) => { host.groupClose(gid); return res; });
	};
	globalThis.pipeline = (items, ...stages) => {
		if (!Array.isArray(items)) throw new TypeError("pipeline: expected an array of items");
		if (stages.length === 0) throw new TypeError("pipeline: needs items and at least one stage");
		if (items.length > host.maxFanout) throw new RangeError("pipeline: more than " + host.maxFanout + " elements - split the work into smaller batches");
		const gid = host.groupOpen("pipeline");
		const one = async (item, index) => {
			let prev = item;
			for (const stage of stages) prev = await stage(prev, item, index);
			return prev;
		};
		return Promise.all(items.map((item, i) => one(item, i).catch(() => null))).then((res) => { host.groupClose(gid); return res; });
	};
})(__wfhost);`

// setupVM builds the engine's goja Runtime: the script-facing globals (agent / phase /
// log / budget / parallel / pipeline via bootstrap), the determinism lockdown, the
// rejection tracker, and the cached JSON.parse / Error handles. Called once per run,
// before any script statement executes.
func (e *engine) setupVM() error {
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	e.vm = vm
	e.unhandled = map[*goja.Promise]goja.Value{}
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, op goja.PromiseRejectionOperation) {
		switch op {
		case goja.PromiseRejectionReject:
			e.unhandled[p] = p.Result()
		case goja.PromiseRejectionHandle:
			delete(e.unhandled, p)
		}
	})
	vm.Set("agent", e.jsAgent)
	vm.Set("phase", e.jsPhase)
	vm.Set("log", e.jsLog)
	vm.Set("budget", newBudgetObject(vm, e))
	host := vm.NewObject()
	_ = host.Set("groupOpen", func(ty string) string { return e.emitGroupOpen(ty) })
	_ = host.Set("groupClose", func(gid string) { e.emitGroupClose(gid) })
	_ = host.Set("maxFanout", maxFanoutElements)
	vm.Set("__wfhost", host)
	if _, err := vm.RunString(bootstrapJS); err != nil {
		return fmt.Errorf("workflow: bootstrap: %w", err)
	}
	glob := vm.GlobalObject()
	_ = glob.Delete("__wfhost")
	_ = glob.Delete("eval")
	_ = glob.Delete("Function")
	jsonObj, ok := vm.Get("JSON").(*goja.Object)
	if !ok {
		return fmt.Errorf("workflow: bootstrap: no JSON global")
	}
	parse, ok := goja.AssertFunction(jsonObj.Get("parse"))
	if !ok {
		return fmt.Errorf("workflow: bootstrap: no JSON.parse")
	}
	e.jsonParse = parse
	stringify, ok := goja.AssertFunction(jsonObj.Get("stringify"))
	if !ok {
		return fmt.Errorf("workflow: bootstrap: no JSON.stringify")
	}
	e.jsonStringify = stringify
	errCtor, ok := vm.Get("Error").(*goja.Object)
	if !ok {
		return fmt.Errorf("workflow: bootstrap: no Error global")
	}
	e.errCtor = errCtor
	e.nestedStub = vm.ToValue(func(call goja.FunctionCall) goja.Value {
		panic(e.newError("workflow: nested workflows are one level deep only"))
	})
	e.workflowFn = vm.ToValue(e.jsWorkflow)
	return nil
}

// newError builds a JS Error value with a clean message — what a Go builtin panics
// with to throw a catchable script exception, and what a completion rejects with.
func (e *engine) newError(format string, a ...any) goja.Value {
	msg := fmt.Sprintf(format, a...)
	obj, err := e.vm.New(e.errCtor, e.vm.ToValue(msg))
	if err != nil {
		return e.vm.ToValue(msg)
	}
	return obj
}

// resolved returns an already-fulfilled Promise — the journal cache-hit path.
func (e *engine) resolved(v goja.Value) goja.Value {
	p, res, _ := e.vm.NewPromise()
	res(v)
	return e.vm.ToValue(p)
}

// promiseSettle carries a leaf promise's resolving functions to its completion's js
// half without naming their goja signatures.
type promiseSettle struct {
	resolve func(goja.Value)
	reject  func(goja.Value)
}

// leafSpec snapshots everything a leaf goroutine needs — plain Go data captured on the
// loop, so the goroutine never touches the VM or the engine state.
type leafSpec struct {
	provider, model, prompt, phase, label string
	key, schemaJSON, isolation, profile   string
	tools                                 []string
	noSkills, mcp                         bool
	timeoutSec, maxBudget                 float64
	maxTurns                              int
	usdEst                                float64
	tokEst                                int64
}

// agentOptKeys is the agent() options contract; an unknown key throws — a weak model's
// typo'd option must fail loudly, not silently no-op.
var agentOptKeys = map[string]bool{
	"provider": true, "model": true, "schema": true, "label": true, "phase": true,
	"timeout": true, "max_budget_usd": true, "max_turns": true,
	"isolation": true, "profile": true, "tools": true, "skills": true, "mcp": true,
}

// jsAgent runs ONE provider subagent leaf and returns a Promise that settles with its
// result. Argument errors, the budget gate, and the lifetime cap THROW synchronously
// (faithful to native — and a throwing thunk inside parallel/pipeline degrades to
// null); a leaf failure REJECTS the promise. With schema the leaf runs with
// --json-schema (claude injects and enforces a forced StructuredOutput tool call); the
// returned payload is validated against the schema as a client backstop — an absent or
// invalid payload rejects (no retry). The prompt is fed via stdin (PromptReader), never
// argv. Runs on the loop: everything up to the goroutine spawn — the journal lookup,
// the budget gate→reserve, the lifetime admit — is atomic with every other builtin.
func (e *engine) jsAgent(call goja.FunctionCall) goja.Value {
	prompt, ok := call.Argument(0).Export().(string)
	if !ok {
		panic(e.newError("agent: prompt must be a string"))
	}
	o := e.agentOpts(call.Argument(1))
	provider := o.str("provider")
	if provider == "" {
		// Provider-less leaf: consume the run's mint-fixed default (recorded in the
		// manifest, so a resume keys on the SAME provider — never a live re-resolve).
		// The recorded NAME is used as-is; if the provider was since removed/disabled
		// it fails through the normal provider validation in subagent.Run AFTER the
		// journal lookup, so a completed leaf still cache-hits on resume. When no
		// default resolved at mint, throw the recorded error_code.
		if e.defaultProvider != "" {
			provider = e.defaultProvider
		} else {
			panic(e.newError("agent: opts.provider omitted and no default provider (%s)", e.defaultProviderErr))
		}
	}
	isolation := o.str("isolation")
	if isolation != "" && isolation != "worktree" {
		panic(e.newError("agent: isolation must be 'worktree' (or omitted), got %q", isolation))
	}
	model := o.str("model")
	label := o.str("label")
	phaseArg := o.str("phase")
	timeoutSec := o.num("timeout")
	maxBudget := o.num("max_budget_usd")
	maxTurns := o.integer("max_turns")
	profile := o.strDefault("profile", subagent.ProfileSlim)
	tools := o.strList("tools")
	skills := o.boolDefault("skills", true)
	mcp, mcpPresent := o.boolean("mcp")
	// Front-load the same slim validation the bare-CLI path uses, surfaced as thrown
	// errors (consistent with the other option errors above): the profile enum, the
	// slim-only refinements rejected when combined with full, and tool canonicalization.
	if perr := subagent.ValidateProfile(profile); perr != nil {
		panic(e.newError("agent: %v", perr))
	}
	isFull := profile == "" || profile == subagent.ProfileFull
	if isFull && (len(tools) > 0 || !skills || mcpPresent) {
		panic(e.newError("agent: tools / skills / mcp are slim-only; they require profile: 'slim' or 'slim-ro'"))
	}
	canonTools, err := canonicalizeTools(tools)
	if err != nil {
		panic(e.newError("agent: %v", err))
	}
	if err := subagent.ValidateToolsSkills(canonTools, !skills); err != nil {
		panic(e.newError("agent: %v", err))
	}

	phaseTag := phaseArg
	if phaseTag == "" {
		phaseTag = e.currentPhase
	}
	// meta.model is the default model for agents that omit model. Apply it BEFORE the
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
	// explicit tools when given, else the profile default (DefaultSlimTools, canonicalized).
	// Folding the resolved set — and passing the SAME set to the leaf via Request.Tools —
	// keeps keying and execution from diverging (a bare slim leaf keys with nil tools while
	// running DefaultSlimTools otherwise). For a full effective profile the slim fields don't
	// fold and Run ignores Tools, so the resolved set is inert there.
	keyTools := canonTools
	if (effProfile == subagent.ProfileSlim || effProfile == subagent.ProfileSlimRO) && len(keyTools) == 0 {
		keyTools, err = subagent.CanonicalizeTools(subagent.DefaultSlimTools(effProfile, !skills))
		if err != nil {
			panic(e.newError("agent: %v", err))
		}
	}
	// MCP per-profile default, resolved in the same pre-keying window as the tool set: an
	// explicit mcp wins; else slim inherits the host config (native generic parity) and
	// slim-ro stays strict. Inert for an effective-full profile (not folded, not emitted).
	if !mcpPresent {
		mcp = effProfile == subagent.ProfileSlim
	}
	var schemaJSON string
	if sv := o.val("schema"); sv != nil {
		sj, serr := e.canonicalSchemaJSON(sv)
		if serr != nil {
			panic(e.newError("agent: schema: %v", serr))
		}
		schemaJSON = sj
	}

	// Log the version-gate downgrade BEFORE the journal lookup, so it is visible even when
	// a cache hit returns without executing (the only place the leaf shape would otherwise
	// be invisible). Routed through log() — the engine's user-visible narrator line.
	if downgrade != "" {
		e.logf("agent(%s): %s; running full", provider, downgrade)
	}

	// Resume replay: a journaled leaf returns an already-resolved promise with NO provider
	// exec, NO slot, and NO lifetime-admit — a cache hit is free. The key spans the
	// result's full determinant (provider / model / base prompt / schema / effective slim
	// shape). A schema leaf re-decodes + re-validates the cached raw answer
	// (deterministic: it passed before); a corrupt/hand-edited entry that fails falls
	// through to re-run the leaf rather than abort the run.
	key := journalKey(provider, model, prompt, schemaJSON, isolation, effProfile, keyTools, !skills, mcp)
	if cached, hit := e.journal.lookup(key); hit {
		if schemaJSON == "" {
			e.emitLeaf("cached", phaseTag, label, provider, model)
			return e.resolved(e.vm.ToValue(cached))
		}
		if v, verr := e.replyToJS(cached, schemaJSON); verr == nil {
			e.emitLeaf("cached", phaseTag, label, provider, model)
			return e.resolved(v)
		}
	}

	// Budget gate: a real exec is about to spend, so refuse once a cap would be breached.
	// Placed AFTER the journal lookup (a cache hit is free and never blocked). A leaf
	// RESERVES a pessimistic per-leaf estimate against each cap so a concurrent fan-out
	// admits against spent+reserved — not spent alone — and can't overshoot the cap by the
	// whole in-flight set; the estimate reconciles to real on completion. The USD estimate
	// over-counts a typical leaf (its own max_budget_usd wins when larger); the token
	// estimate is the flat per-leaf floor. Gate→reserve runs uninterrupted on the loop, so
	// a parallel fan-out's leaves serialize through it. First cap to trip aborts.
	usdEst := defaultLeafEstimate
	if maxBudget > usdEst {
		usdEst = maxBudget
	}
	tokEst := int64(defaultLeafTokenEstimate)
	if e.budgetWouldExceed(usdEst, tokEst) {
		panic(e.newError("%v", e.budgetExceededErr()))
	}
	if !e.sched.admit() {
		panic(e.newError("agent: run exceeded the %d-leaf lifetime cap", maxLifetimeAgents))
	}
	e.budgetReserve(usdEst, tokEst)

	spec := leafSpec{
		provider: provider, model: model, prompt: prompt, phase: phaseTag, label: label,
		key: key, schemaJSON: schemaJSON, isolation: isolation, profile: profile,
		tools: keyTools, noSkills: !skills, mcp: mcp,
		timeoutSec: timeoutSec, maxBudget: maxBudget, maxTurns: maxTurns,
		usdEst: usdEst, tokEst: tokEst,
	}
	// Mint the queued placeholder ON the loop (one small file write, the same class as
	// the on-loop journal/manifest writes) so the control plane can address the leaf by
	// job id from the moment it exists; the attempt goroutines reuse the id.
	jobID := mintQueuedLeaf(subagent.Request{
		Provider: provider, RunID: e.runID, Phase: phaseTag, Label: label,
		JournalKey: key, PersistIO: e.persistIO, PromptProfile: profile,
	}, model)
	p, resolve, reject := e.vm.NewPromise()
	h := &leafCtl{
		spec: spec,
		settle: promiseSettle{
			resolve: func(v goja.Value) { resolve(v) },
			reject:  func(v goja.Value) { reject(v) },
		},
		gen: 1,
	}
	if jobID != "" {
		e.ctl[jobID] = h // "" (a mint hiccup / the test stub) → uncontrollable, still runs
	}
	e.inflight++
	// The held-phase registration gate (post-mint, post-cache-lookup: a cached replay
	// under a held phase still serves — "stop a phase" stops execution, not value flow):
	// a leaf minted into a held phase parks before its first exec, exactly like a held
	// leaf, and a phase restart wakes it.
	if jobID != "" && e.heldPhases[phaseTag] {
		subagent.HoldLeaf(jobID)
		h.held = true
		e.emitLeaf("held", phaseTag, label, provider, model)
		return e.vm.ToValue(p)
	}
	e.spawnAttempt(jobID, h)
	return e.vm.ToValue(p)
}

// spawnAttempt launches one attempt of a leaf: a fresh cancellable ctx (the stop
// directive's kill handle) under the run's leaf ctx, and the goroutine that runs the
// exec and posts the attempt's completion. Loop-held caller.
func (e *engine) spawnAttempt(jobID string, h *leafCtl) {
	ctx, cancel := context.WithCancel(e.leafCtx)
	h.cancel = cancel
	h.spawned = true
	gen := h.gen
	go func() {
		res, preErr := e.execLeaf(ctx, jobID, h, h.spec)
		e.completeAttempt(jobID, gen, h, res, preErr)
	}()
}

// execLeaf is one attempt's goroutine body: it runs the leaf to a single (res, preErr)
// outcome — recovering a leaf panic into a failure so a thunk's leaf can never crash
// the process. It touches no engine state; the attempt's completion (on the loop)
// dispatches directives and settles budgets, journal, events, and the job correction.
func (e *engine) execLeaf(ctx context.Context, jobID string, h *leafCtl, spec leafSpec) (res subagent.Result, preErr error) {
	defer func() {
		if r := recover(); r != nil {
			res = subagent.Result{}
			preErr = fmt.Errorf("agent(%s): leaf panicked: %v", spec.provider, r)
		}
	}()
	if perr := ensureLeafProxy(spec.provider); perr != nil {
		return subagent.Result{}, fmt.Errorf("agent(%s): codex proxy unavailable: %v", spec.provider, perr)
	}
	// Acquire a pool slot; the slot is held ONLY across this attempt's actual exec and
	// released right after — never across anything a script branch nests — so nesting
	// can't deadlock on a slot a parent branch is sitting on. A cancel while queued is
	// a STOP, not a failure: the stopped-class result holds the leaf (a directive) or
	// finalizes it "stopped" (a run abort), uniform with a killed in-flight attempt.
	if !e.sched.acquireSlot(ctx) {
		stopped := subagent.Result{ErrorCode: subagent.ErrCodeStopped, ErrorMsg: "run stopped before the leaf launched"}
		return stopped, fmt.Errorf("agent: run cancelled before launch")
	}
	defer e.sched.releaseSlot()
	// Worktree isolation: run the attempt with cwd = a fresh git worktree, torn down on
	// return (success, failure, or panic).
	workDir := ""
	if spec.isolation == "worktree" {
		dir, cleanup, werr := createWorktreeFn(e.runID)
		if werr != nil {
			return subagent.Result{}, fmt.Errorf("agent: %v", werr)
		}
		defer cleanup()
		workDir = dir
	}
	e.post(leafCB{state: func() {
		e.emitLeaf("launch", spec.phase, spec.label, spec.provider, spec.model)
	}})
	res = runLeaf(ctx, subagent.Request{
		Provider:       spec.provider,
		Model:          spec.model,
		PromptReader:   strings.NewReader(spec.prompt), // stdin, not argv
		JSON:           true,                           // force inner json → res.Result is the answer text
		Timeout:        time.Duration(spec.timeoutSec * float64(time.Second)),
		MaxTurns:       spec.maxTurns,
		MaxBudgetUSD:   spec.maxBudget,
		RunID:          e.runID,
		Phase:          spec.phase,
		Label:          spec.label,
		JobID:          jobID, // reuse the minted job: one row, queued→running→held/terminal across attempts
		Attempt:        h.gen,
		JournalKey:     spec.key,
		PersistIO:      e.persistIO,
		StreamActivity: e.persistIO, // sync leaf streams tool/usage activity for the board (gated like PersistIO)
		IOPrompt:       spec.prompt, // persisted only when PersistIO (subagent gates)
		WorkingDir:     workDir,     // empty unless isolation: 'worktree'
		// Slim profile: Run re-resolves the EFFECTIVE profile (its own version gate); the
		// REQUESTED profile is passed here, the engine's effProfile only keys + logs.
		PromptProfile: spec.profile,
		Tools:         spec.tools, // the resolved key set — the leaf execs exactly what was keyed
		NoSkills:      spec.noSkills,
		MCP:           spec.mcp,
		JSONSchema:    spec.schemaJSON,
	})
	return res, nil
}

// completeAttempt posts one attempt's completion. The state half (always runs, on the
// loop) is the ctl-aware DISPATCHER: it consults the leaf's armed directive FIRST —
// hold and respawn keep the in-flight count and the frame's reservation — and only the
// final-settle path does the terminal accounting (journal, events, job correction,
// release) and arms the js half, which settles the promise (skipped once the run is
// aborted). Success-wins: an OK result (or a genuine non-stopped failure under a
// restart) means the attempt finished before the kill landed — the directive is dropped
// and narrated, never applied to a result that already exists.
func (e *engine) completeAttempt(jobID string, gen int, h *leafCtl, res subagent.Result, preErr error) {
	var out struct {
		err     error
		payload string
		schema  bool
		settle  bool
	}
	spec := h.spec
	state := func() {
		if gen != h.gen {
			e.logf("control: leaf %s attempt %d completed after attempt %d superseded it; dropped", jobID, gen, h.gen)
			return
		}
		stoppedClass := res.ErrorCode == subagent.ErrCodeStopped
		if !e.aborted && h.pending != "" {
			directive := h.pending
			h.pending = ""
			switch {
			case directive == "stop" && (stoppedClass || !res.OK):
				// The kill (or a failure dying under it) took effect: HOLD. The meta is
				// already held (pre-marked) and the terminal cache suppressed; the
				// promise stays unsettled and inflight/reservation are kept.
				h.held = true
				h.cancel = nil
				e.emitLeaf("held", spec.phase, spec.label, spec.provider, spec.model)
				return
			case directive == "restart" && (stoppedClass || !res.OK):
				if e.budgetWouldExceed(0, 0) {
					e.logf("control: leaf %s restart refused — %v; the leaf stays held", jobID, e.budgetExceededErr())
					h.held = true
					h.cancel = nil
					e.emitLeaf("held", spec.phase, spec.label, spec.provider, spec.model)
					return
				}
				if !e.sched.admit() {
					e.logf("control: leaf %s restart refused — the %d-leaf lifetime cap is exhausted; the leaf stays held", jobID, maxLifetimeAgents)
					h.held = true
					h.cancel = nil
					e.emitLeaf("held", spec.phase, spec.label, spec.provider, spec.model)
					return
				}
				h.gen++
				subagent.RequeueLeaf(jobID, h.gen)
				e.spawnAttempt(jobID, h)
				return
			default:
				// success-wins: the real outcome stands. finalizeSyncJob normalized a
				// pre-mark it observed; a directive that landed AFTER the finalize left
				// a held meta under the terminal cache — clear it (cache-first).
				subagent.NormalizeHeldLeaf(jobID)
				e.logf("control: leaf %s finished before the %s applied; result kept", jobID, directive)
			}
		}
		defer e.releaseLeaf(jobID, h)
		if preErr != nil {
			subagent.FinalizeQueuedLeafFailed(jobID, res)
			e.emitLeaf(failureEventStatus(res), spec.phase, spec.label, spec.provider, spec.model)
			out.err = preErr
			return
		}
		if !res.OK {
			// A pre-flight fail (no Run registration) keeps its real error class on the job.
			subagent.FinalizeQueuedLeafFailed(jobID, res)
			e.emitLeaf(failureEventStatus(res), spec.phase, spec.label, spec.provider, spec.model)
			out.err = fmt.Errorf("agent(%s): %s: %s", spec.provider, res.ErrorCode, res.ErrorMsg)
			return
		}
		// Book the exec's real cost: USD (claude's list-price estimate) + tokens
		// (input+output). The frame's reservation is freed by releaseLeaf above;
		// charging keeps spent+reserved monotonic for concurrent gates.
		e.budgetCharge(res.CostUSD, leafTokens(res))
		if spec.schemaJSON == "" {
			e.journal.append(spec.key, res.Result)
			e.emitLeaf("done", spec.phase, spec.label, spec.provider, spec.model)
			out.payload = res.Result
			out.settle = true
			return
		}
		// Schema leaf: claude enforced that the StructuredOutput tool was CALLED; the
		// client validation below is the backstop for a weak provider filling it invalidly.
		// An OK envelope WITHOUT the payload (e.g. a max_turns-starved leaf) is a failure
		// — never a prose-JSON fallback. Validation failure is terminal: an identical
		// re-run reproduces it at full leaf cost.
		if len(res.StructuredOutput) == 0 {
			subagent.FinalizeQueuedLeafFailed(jobID, subagent.Result{})
			e.emitLeaf("failed", spec.phase, spec.label, spec.provider, spec.model)
			out.err = fmt.Errorf("agent(%s): schema: no structured_output in the result envelope (the StructuredOutput call costs turns — raise max_turns)", spec.provider)
			return
		}
		if _, verr := decodeAndValidate(string(res.StructuredOutput), spec.schemaJSON); verr != nil {
			subagent.FinalizeQueuedLeafFailed(jobID, subagent.Result{})
			e.emitLeaf("failed", spec.phase, spec.label, spec.provider, spec.model)
			out.err = fmt.Errorf("agent(%s): schema not satisfied: %v", spec.provider, verr)
			return
		}
		e.journal.append(spec.key, string(res.StructuredOutput))
		e.emitLeaf("done", spec.phase, spec.label, spec.provider, spec.model)
		out.payload, out.schema = string(res.StructuredOutput), true
		out.settle = true
	}
	js := func() {
		if out.err != nil {
			h.settle.reject(e.newError("%v", out.err))
			return
		}
		if !out.settle {
			return
		}
		if !out.schema {
			h.settle.resolve(e.vm.ToValue(out.payload))
			return
		}
		v, perr := e.jsonParse(goja.Undefined(), e.vm.ToValue(stripCodeFence(out.payload)))
		if perr != nil {
			h.settle.reject(e.newError("agent(%s): schema payload is not valid JSON: %v", spec.provider, perr))
			return
		}
		h.settle.resolve(v)
	}
	e.post(leafCB{state: state, js: js})
}

// failureEventStatus maps a failed attempt's result class to its board event: a
// stopped-class kill is "stopped" (terminal-only vocabulary — a hold emits "held"
// upstream instead), everything else "failed".
func failureEventStatus(res subagent.Result) string {
	if res.ErrorCode == subagent.ErrCodeStopped {
		return "stopped"
	}
	return "failed"
}

// jsonClone deep-copies a JS value through the VM's stringify→parse pair, yielding pure
// data with JS JSON semantics (undefined/function/symbol members drop; a top-level
// non-serializable value or a cycle errors). Loop-held caller.
func (e *engine) jsonClone(v goja.Value) (goja.Value, error) {
	sv, err := e.jsonStringify(goja.Undefined(), v)
	if err != nil {
		return nil, err
	}
	s, ok := sv.Export().(string)
	if !ok {
		return nil, fmt.Errorf("value is not JSON-serializable")
	}
	return e.jsonParse(goja.Undefined(), e.vm.ToValue(s))
}

// replyToJS validates a (cached) schema reply Go-side, then materializes it as a plain
// JS value via the VM's own JSON.parse. Loop-held caller.
func (e *engine) replyToJS(raw, schemaJSON string) (goja.Value, error) {
	if _, err := decodeAndValidate(raw, schemaJSON); err != nil {
		return nil, err
	}
	v, err := e.jsonParse(goja.Undefined(), e.vm.ToValue(stripCodeFence(raw)))
	if err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %v", err)
	}
	return v, nil
}

// jsPhase sets the run's current phase (used to tag agents that don't pass phase) and
// records the title on the manifest in first-seen order (live board ordering).
// Best-effort: a manifest hiccup never fails the run. Loop-held, so the in-memory
// phase update and the full manifest overwrite are serialized.
func (e *engine) jsPhase(call goja.FunctionCall) goja.Value {
	title, ok := call.Argument(0).Export().(string)
	if !ok {
		panic(e.newError("phase: title must be a string"))
	}
	detail := ""
	if v := call.Argument(1); !goja.IsUndefined(v) && !goja.IsNull(v) {
		d, ok := v.Export().(string)
		if !ok {
			panic(e.newError("phase: detail must be a string"))
		}
		detail = d
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
	return goja.Undefined()
}

// jsLog writes a narrator line to stderr (diagnostic — discarded when the run is
// detached, visible with --foreground) AND emits a live-event record that `workflow
// watch` renders. stdout stays clean for the run id the launcher prints; the stderr
// stream itself is not persisted.
func (e *engine) jsLog(call goja.FunctionCall) goja.Value {
	msg, ok := call.Argument(0).Export().(string)
	if !ok {
		panic(e.newError("log: msg must be a string"))
	}
	fmt.Fprintln(os.Stderr, "[workflow] "+msg)
	e.events.emit(EventRecord{Kind: "log", Msg: msg})
	return goja.Undefined()
}

// emitLeaf records a leaf transition (launch/done/failed/cached) on the live-event
// channel. Loop-held callers only; nil-safe via the writer.
func (e *engine) emitLeaf(status, phase, label, provider, model string) {
	e.events.emit(EventRecord{Kind: "leaf", Status: status, Phase: phase, Label: label, Provider: provider, Model: model})
}

// logf is the engine-internal narrator: a formatted line to stderr plus a `log`
// live-event, the same surface the log() builtin exposes to scripts. Used for the slim
// version-gate downgrade notice. Loop-held callers only.
func (e *engine) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, "[workflow] "+msg)
	e.events.emit(EventRecord{Kind: "log", Msg: msg})
}

// emitGroupOpen records the start of a parallel/pipeline/workflow group and returns its
// id; emitGroupClose records its end. `workflow watch` brackets the group in its live
// stream by seq order: every event between an open and its matching close belongs to
// that group, and nested groups bracket by order. The open runs in the builtin (before
// any element executes); the close runs in the group promise's settlement reaction —
// both on the loop.
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
// never fails the run. Loop-held callers only (plus Execute's pre-run stamp + deferred
// finalize, when the loop is not running) — manifest writes never race.
func (e *engine) saveManifest(status, errText string) {
	_ = subagent.SaveRun(subagent.WorkflowRun{
		RunID:           e.runID,
		Name:            e.name,
		Description:     e.description,
		WhenToUse:       e.whenToUse,
		StartedAt:       e.startedAt,
		UpdatedAt:       time.Now().UTC().Format(time.RFC3339),
		Phases:          e.phases,
		Status:          status,
		Error:           errText,
		EnginePID:       e.enginePID,
		EngineProcStart: e.engineProcStart,
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
		// The mint-fixed default resolution: re-persisted so a resume reads the SAME
		// provider a provider-less leaf already keyed on (never a live re-resolve).
		DefaultProvider:      e.defaultProvider,
		DefaultProviderError: e.defaultProviderErr,
	})
}

// canonicalizeTools validates + canonicalizes an explicit tools set (dedupe + sort) so
// caller order never changes the journal key; an empty set stays nil (the profile
// default applies in subagent.Run). Delegates to the single canonical validator so the
// engine and bare-CLI paths reject identically.
func canonicalizeTools(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	return subagent.CanonicalizeTools(names)
}

// jsOpts wraps the agent() options object for strict field reads: an absent, undefined,
// or null field reads as the documented default; a wrong type throws, mirroring the old
// kwarg errors; an unknown key throws (agentOptKeys).
type jsOpts struct {
	e   *engine
	obj *goja.Object
}

func (e *engine) agentOpts(v goja.Value) jsOpts {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return jsOpts{e: e}
	}
	obj, ok := v.(*goja.Object)
	if !ok {
		panic(e.newError("agent: opts must be an object, got %s", jsTypeName(v)))
	}
	for _, k := range obj.Keys() {
		if !agentOptKeys[k] {
			panic(e.newError("agent: unknown option %q", k))
		}
	}
	return jsOpts{e: e, obj: obj}
}

// val returns the named option, or nil when absent / undefined / null (the documented
// "omitted" forms — a script may copy the signature's `model: null` verbatim).
func (o jsOpts) val(name string) goja.Value {
	if o.obj == nil {
		return nil
	}
	v := o.obj.Get(name)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	return v
}

func (o jsOpts) str(name string) string {
	v := o.val(name)
	if v == nil {
		return ""
	}
	s, ok := v.Export().(string)
	if !ok {
		panic(o.e.newError("agent: %s must be a string, got %s", name, jsTypeName(v)))
	}
	return s
}

func (o jsOpts) strDefault(name, def string) string {
	v := o.val(name)
	if v == nil {
		return def
	}
	s, ok := v.Export().(string)
	if !ok {
		panic(o.e.newError("agent: %s must be a string, got %s", name, jsTypeName(v)))
	}
	return s
}

// num coerces a numeric option to float64 (int64 and float64 exports both accepted, so
// a script writes the natural timeout: 120 or max_budget_usd: 1.5). Non-finite values
// are rejected — an Infinity reservation would corrupt the budget arithmetic.
func (o jsOpts) num(name string) float64 {
	v := o.val(name)
	if v == nil {
		return 0
	}
	switch n := v.Export().(type) {
	case int64:
		return float64(n)
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			panic(o.e.newError("agent: %s must be a finite number", name))
		}
		return n
	}
	panic(o.e.newError("agent: %s must be a number, got %s", name, jsTypeName(v)))
}

func (o jsOpts) integer(name string) int {
	v := o.val(name)
	if v == nil {
		return 0
	}
	n, ok := v.Export().(int64)
	if !ok {
		panic(o.e.newError("agent: %s must be an integer, got %s", name, jsTypeName(v)))
	}
	return int(n)
}

func (o jsOpts) boolDefault(name string, def bool) bool {
	v := o.val(name)
	if v == nil {
		return def
	}
	b, ok := v.Export().(bool)
	if !ok {
		panic(o.e.newError("agent: %s must be a boolean, got %s", name, jsTypeName(v)))
	}
	return b
}

// boolean reads an explicit boolean option, reporting presence — so mcp reads its
// per-profile default only when the script truly didn't choose.
func (o jsOpts) boolean(name string) (val, present bool) {
	v := o.val(name)
	if v == nil {
		return false, false
	}
	b, ok := v.Export().(bool)
	if !ok {
		panic(o.e.newError("agent: %s must be a boolean, got %s", name, jsTypeName(v)))
	}
	return b, true
}

func (o jsOpts) strList(name string) []string {
	v := o.val(name)
	if v == nil {
		return nil
	}
	lst, ok := v.Export().([]interface{})
	if !ok {
		panic(o.e.newError("agent: %s must be an array of strings, got %s", name, jsTypeName(v)))
	}
	out := make([]string, 0, len(lst))
	for i, el := range lst {
		s, ok := el.(string)
		if !ok {
			panic(o.e.newError("agent: %s[%d] must be a string", name, i))
		}
		out = append(out, s)
	}
	return out
}

// jsTypeName names a JS value's type for error messages.
func jsTypeName(v goja.Value) string {
	switch t := v.Export().(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64, float64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		_ = t
		return "function"
	}
}
