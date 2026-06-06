package workflow

import (
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// bgPollInterval is how often await() polls a background leaf's job status. Background
// leaves are multi-second vendor execs, so a coarse poll keeps fs load low.
const bgPollInterval = 250 * time.Millisecond

// defaultBgBackstop bounds an awaited background leaf the script launched with no timeout=. It is a
// safety net against an infinitely stalled leaf (a stalled result stream with no process exit), NOT a
// task-length budget — deliberately large so it rarely trips on a legitimately long background task (one
// that genuinely runs past it is reaped: a stall and a slow task are indistinguishable by wall-clock).
// An overrun is reaped at wait() (resolveHandle → ReapJob → SUBAGENT_TIMEOUT).
const defaultBgBackstop = 45 * time.Minute

// statusForFn is the background-job poll — a seam so tests inject a deterministic status
// in place of reading real job files. Production = subagent.StatusFor.
var statusForFn = subagent.StatusFor

// bgHandle is what agent(run_in_background=True) returns: an opaque handle the script
// passes to await(). It carries the detached leaf's job id and the content key (so its
// result is journaled at await time), or — on resume — a result already served from the
// journal (resolved=true), in which case await never spawns. It is a Starlark value but
// not a container; its fields are set once and only read, so Freeze is a no-op and it is
// safe to hold across the GIL.
type bgHandle struct {
	jobID    string
	key      string
	resolved bool      // result already known (journal cache hit on resume)
	cached   string    // the cached result, when resolved
	deadline time.Time // wall-clock timeout enforced at wait() (zero = none); the launch itself is deadline-less
	// est/estTok carry the leaf's budget RESERVATION from launchBg through to resolveHandle, which
	// releases it (and charges the real cost) at await. A resolved handle (journal hit) never
	// reserved, so both stay 0 and its release is a no-op.
	est    float64
	estTok int64
	// display/event tags
	vendor, model, phase, label string
}

var _ starlark.Value = (*bgHandle)(nil)

func (h *bgHandle) String() string        { return "agent-handle" }
func (h *bgHandle) Type() string          { return "agent-handle" }
func (h *bgHandle) Freeze()               {}
func (h *bgHandle) Truth() starlark.Bool  { return starlark.True }
func (h *bgHandle) Hash() (uint32, error) { return 0, fmt.Errorf("agent-handle is unhashable") }

// slimReq carries the slim-profile knobs from agent() into launchBg: the REQUESTED
// profile (subagent.Run re-resolves the effective one) and its resolved refinements.
type slimReq struct {
	profile  string
	tools    []string
	noSkills bool
	mcp      bool
}

// launchBg starts a leaf detached (subagent --background) and returns a handle. It admits
// against the lifetime cap but takes NO live-pool slot (a detached leaf isn't bounded by
// the in-process pool); its result is journaled at await time, not now. It owns the budget
// RESERVATION for the detached leaf: it reserves (usdEst, tokEst) before the launch, releases on a
// launch failure, and carries the estimate on the handle so resolveHandle frees it (and charges
// real) at await. GIL-held caller.
func (e *engine) launchBg(vendor, model, prompt, phaseTag, label, key string, timeoutSec, maxBudget float64, maxTurns int, usdEst float64, tokEst int64, slim slimReq) (starlark.Value, error) {
	if !e.sched.admit() {
		return nil, fmt.Errorf("agent: run exceeded the %d-leaf lifetime cap", maxLifetimeAgents)
	}
	e.budgetReserve(usdEst, tokEst)
	e.emitLeaf("launch", phaseTag, label, vendor, model)
	var res subagent.Result
	e.sched.runBlocking(func() {
		res = runLeaf(subagent.Request{
			Vendor:        vendor,
			Model:         model,
			PromptReader:  strings.NewReader(prompt),
			JSON:          true,
			Background:    true,
			Timeout:       time.Duration(timeoutSec * float64(time.Second)),
			MaxTurns:      maxTurns,
			MaxBudgetUSD:  maxBudget,
			RunID:         e.runID,
			Phase:         phaseTag,
			Label:         label,
			JournalKey:    key, // persisted so the board can restart THIS leaf (invalidate + resume)
			PersistIO:     e.persistIO,
			IOPrompt:      prompt,
			PromptProfile: slim.profile, // Run re-resolves the effective profile; engine keys it separately
			Tools:         slim.tools,
			NoSkills:      slim.noSkills,
			MCP:           slim.mcp,
		})
	})
	if !res.OK {
		e.budgetRelease(usdEst, tokEst) // launch failed → no handle will carry the reservation; free it here
		e.emitLeaf("failed", phaseTag, label, vendor, model)
		return nil, fmt.Errorf("agent(%s): background launch: %s: %s", vendor, res.ErrorCode, res.ErrorMsg)
	}
	h := &bgHandle{jobID: res.JobID, key: key, est: usdEst, estTok: tokEst, vendor: vendor, model: model, phase: phaseTag, label: label}
	// A detached background job outlives the launcher, so its timeout is enforced at wait() (resolveHandle
	// reaps an overrun); with no script timeout it falls back to the backstop so an AWAITED leaf can never
	// poll forever.
	h.deadline = bgDeadline(time.Now(), timeoutSec)
	return h, nil
}

// bgDeadline is the wall-clock deadline for an awaited background leaf: the script timeout when
// positive, else the default backstop (so an awaited leaf can never poll forever).
func bgDeadline(now time.Time, timeoutSec float64) time.Time {
	if timeoutSec <= 0 {
		return now.Add(defaultBgBackstop)
	}
	return now.Add(time.Duration(timeoutSec * float64(time.Second)))
}

// await blocks for one handle or a list of handles and returns their result string(s) —
// a bare handle returns a string, a list returns a list (order preserved). Each handle's
// result is journaled on first completion, so a later resume serves it from the journal
// without re-running.
func (e *engine) await(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var arg starlark.Value
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &arg); err != nil {
		return nil, err
	}
	if h, ok := arg.(*bgHandle); ok {
		return e.resolveHandle(h)
	}
	it, ok := arg.(starlark.Iterable)
	if !ok {
		return nil, fmt.Errorf("wait: expected an agent-handle or a list of them, got %s", arg.Type())
	}
	iter := it.Iterate()
	defer iter.Done()
	var out []starlark.Value
	var x starlark.Value
	for iter.Next(&x) {
		h, ok := x.(*bgHandle)
		if !ok {
			return nil, fmt.Errorf("wait: list element is %s, not an agent-handle", x.Type())
		}
		r, err := e.resolveHandle(h)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return starlark.NewList(out), nil
}

// resolveHandle returns a background leaf's result, polling its detached job to terminal
// (GIL released during the poll, like a sync leaf's exec) and journaling it on success. A
// resolved handle (journal hit on resume) returns immediately with no poll. A failed leaf
// is NOT journaled, so a resume re-launches it. GIL-held caller.
func (e *engine) resolveHandle(h *bgHandle) (starlark.Value, error) {
	if h.resolved {
		e.emitLeaf("cached", h.phase, h.label, h.vendor, h.model)
		return starlark.String(h.cached), nil
	}
	// Free the leaf's budget reservation on EVERY terminal exit below (timeout / cancel / fail /
	// success); success charges its real cost first. Registered AFTER the resolved short-circuit
	// (a resolved handle never reserved), and runs GIL-held on the unwind. Zero the estimate after
	// releasing so awaiting the SAME handle again (its failure paths don't set resolved) can't double-
	// release and free another leaf's reservation.
	defer func() {
		e.budgetRelease(h.est, h.estTok)
		h.est, h.estTok = 0, 0
	}()
	var res subagent.Result
	cancelled, timedOut := false, false
	e.sched.runBlocking(func() {
		for {
			res = statusForFn(h.jobID)
			if res.Status != "running" {
				return
			}
			if !h.deadline.IsZero() && time.Now().After(h.deadline) {
				timedOut = true
				return
			}
			select {
			case <-e.sched.ctx.Done(): // a stop / Ctrl-C must not block on the detached job
				cancelled = true
				return
			case <-time.After(bgPollInterval):
			}
		}
	})
	if timedOut {
		_ = subagent.ReapJob(h.jobID) // enforce the timeout: terminate the overrunning leaf
		e.emitLeaf("failed", h.phase, h.label, h.vendor, h.model)
		return nil, fmt.Errorf("agent(%s, background): timed out", h.vendor)
	}
	if cancelled {
		e.emitLeaf("failed", h.phase, h.label, h.vendor, h.model)
		return nil, fmt.Errorf("agent(%s, background): run cancelled while awaiting", h.vendor)
	}
	if !res.OK {
		e.emitLeaf("failed", h.phase, h.label, h.vendor, h.model)
		return nil, fmt.Errorf("agent(%s, background): %s: %s", h.vendor, res.ErrorCode, res.ErrorMsg)
	}
	e.budgetCharge(res.CostUSD, leafTokens(res))
	e.journal.append(h.key, res.Result)
	// Mark resolved so a second wait(h) returns the cached result instead of re-polling,
	// double-counting cost, and appending a duplicate journal entry.
	h.resolved = true
	h.cached = res.Result
	e.emitLeaf("done", h.phase, h.label, h.vendor, h.model)
	return starlark.String(res.Result), nil
}
