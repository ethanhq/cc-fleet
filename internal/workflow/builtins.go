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
// None at that index). With schema= it appends a JSON instruction, parses + shallowly
// validates the reply, and retries up to twice. The prompt is fed via stdin
// (PromptReader), never argv. Entered + returns with the GIL held; the GIL is
// released only for the blocking exec + slot wait (via runBlocking).
func (e *engine) agent(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var (
		prompt     string
		vendor     string
		modelVal   starlark.Value
		schemaVal  starlark.Value
		labelVal   starlark.Value
		phaseVal   starlark.Value
		timeoutVal starlark.Value
		budgetVal  starlark.Value
		turnsVal   starlark.Value
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
	); err != nil {
		return nil, err
	}
	if vendor == "" {
		return nil, fmt.Errorf("agent: vendor= is required")
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

	// CONVERT-UNDER-LOCK: snapshot every Starlark input into Go data while the GIL is
	// held, before releasing it for the blocking exec.
	phaseTag := phaseArg
	if phaseTag == "" {
		phaseTag = e.currentPhase
	}
	var schemaJSON string
	var requiredKeys []string
	if schemaVal != nil && schemaVal != starlark.None {
		sj, keys, serr := encodeSchema(thread, schemaVal)
		if serr != nil {
			return nil, fmt.Errorf("agent: schema: %w", serr)
		}
		schemaJSON, requiredKeys = sj, keys
	}
	if !e.sched.admit() {
		return nil, fmt.Errorf("agent: run exceeded the %d-leaf lifetime cap", maxLifetimeAgents)
	}

	// Acquire a pool slot with the GIL released, so waiting for a slot never pins the
	// interpreter. Only register the slot-release defer AFTER a successful acquire.
	acquired := false
	e.sched.runBlocking(func() { acquired = e.sched.acquireSlot() })
	if !acquired {
		return nil, fmt.Errorf("agent: run cancelled before launch")
	}
	defer e.sched.releaseSlot()

	attempts := 1
	if schemaJSON != "" {
		attempts = 3 // 1 + 2 retries
	}
	sendPrompt := prompt
	if schemaJSON != "" {
		sendPrompt = prompt + jsonInstruction(schemaJSON)
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		req := subagent.Request{
			Vendor:       vendor,
			Model:        model,
			PromptReader: strings.NewReader(sendPrompt), // stdin, not argv
			JSON:         true,                          // force inner json → res.Result is the answer text
			Timeout:      time.Duration(timeoutSec * float64(time.Second)),
			MaxTurns:     maxTurns,
			MaxBudgetUSD: maxBudget,
			RunID:        e.runID,
			Phase:        phaseTag,
			Label:        label,
		}
		var res subagent.Result
		e.sched.runBlocking(func() { res = runLeaf(req) })
		if !res.OK {
			return nil, fmt.Errorf("agent(%s): %s: %s", vendor, res.ErrorCode, res.ErrorMsg)
		}
		if schemaJSON == "" {
			return starlark.String(res.Result), nil
		}
		v, verr := decodeAndValidate(thread, res.Result, requiredKeys)
		if verr == nil {
			return v, nil
		}
		lastErr = verr
		sendPrompt = prompt + "\n\nYour previous reply was rejected: " + verr.Error() + jsonInstruction(schemaJSON)
	}
	return nil, fmt.Errorf("agent(%s): schema not satisfied after %d attempts: %v", vendor, attempts, lastErr)
}

func jsonInstruction(schemaJSON string) string {
	return "\n\nReturn ONLY a single JSON value matching this schema, with no prose and no markdown fences:\n" + schemaJSON
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
	results := make([]starlark.Value, len(thunks))
	e.fanout(len(thunks), func(i int, th *starlark.Thread) {
		results[i] = e.callOrNone(th, thunks[i], nil)
	})
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
	results := make([]starlark.Value, len(items))
	e.fanout(len(items), func(i int, th *starlark.Thread) {
		results[i] = e.runPipelineItem(th, items[i], i, stages)
	})
	return starlark.NewList(results), nil
}

// phase sets the run's current phase (used to tag agents that don't pass phase=) and
// records the title on the manifest in first-seen order (live board ordering).
// Best-effort like the substrate's board bookkeeping: a manifest hiccup never fails
// the run. GIL-held, so the manifest read-modify-write is serialized.
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
	return starlark.None, nil
}

// saveManifest overwrites the run manifest from the engine's authoritative in-memory
// state (errText is recorded only on a failed finalize). Best-effort, like the
// substrate's board bookkeeping: a write hiccup never fails the run. GIL-held callers
// only (phase() and Execute's finalize).
func (e *engine) saveManifest(status, errText string) {
	_ = subagent.SaveRun(subagent.WorkflowRun{
		RunID:       e.runID,
		Name:        e.name,
		Description: e.description,
		StartedAt:   e.startedAt,
		Phases:      e.phases,
		Status:      status,
		Error:       errText,
	})
}

// log emits a narrator line to STDERR (diagnostic), keeping stdout clean for the run
// id the launcher prints. It is discarded when the run is detached (stderr → /dev/null)
// and visible with --foreground. v1 does not persist narrator logs — the board reflects
// the manifest + tagged jobs, and a live activity tail is a v4 concern.
func (e *engine) log(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var msg string
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "msg", &msg); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "[workflow] "+msg)
	return starlark.None, nil
}

// fanout releases the GIL, runs work(i, thread) on a fresh goroutine + thread for each
// i in [0,n), waits for all, then re-acquires the GIL. work runs with the GIL HELD
// (each goroutine acquires it for its starlark.Call) and stores its own result. The
// GIL is released for the whole wait so the goroutines can acquire it; on return it is
// re-acquired so the interpreter resumes single-threaded.
func (e *engine) fanout(n int, work func(i int, th *starlark.Thread)) {
	var wg sync.WaitGroup
	e.sched.unlock()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			th := e.sched.newThread(fmt.Sprintf("workflow:%s:%d", e.runID, i))
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
// is capped at maxLifetimeAgents so a pathological list can't spawn an unbounded number
// of goroutines in fanout (a list that large would exhaust the lifetime cap anyway).
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
		if len(out) >= maxLifetimeAgents {
			return nil, fmt.Errorf("%s: more than %d elements — split the work into smaller batches", fname, maxLifetimeAgents)
		}
		x.Freeze()
		out = append(out, x)
	}
	return out, nil
}
