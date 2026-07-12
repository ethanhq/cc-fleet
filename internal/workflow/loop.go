package workflow

import (
	"errors"
	"fmt"

	"github.com/dop251/goja"
)

// leafCB is one unit of work a leaf goroutine hands back to the engine loop. state
// mutates pure Go engine state (the in-flight count, budget, journal, events, job
// files) and ALWAYS runs; js settles the leaf's promise — executing the script's
// awaiting continuations before it returns — and is SKIPPED once the run is aborted:
// the script is already dead and the VM may be interrupted, and a dropped settlement
// loses nothing durable because the leaf's job file carries its own terminal status.
type leafCB struct {
	state func()
	js    func()
}

// errNoProgress fails a run whose script awaits a promise nothing can ever settle: the
// engine installs no timers, so leaf completion is the only async event source, and
// "script pending + zero leaves in flight + an empty queue" is a proven deadlock —
// e.g. `await new Promise(() => {})`.
var errNoProgress = errors.New("workflow: the script awaits a promise nothing will ever settle (no leaf in flight; the runtime has no timers)")

// errRunStopped classifies a cooperatively-stopped run; the caller finalizes the
// manifest "stopped", not "failed".
var errRunStopped = errors.New("workflow: run stopped")

// post hands a callback from a leaf goroutine to the loop. It never strands a
// goroutine past the loop's lifetime: loopDone unblocks stragglers after an abnormal
// loop exit (the run is already finalizing; their job files hold the terminal truth).
func (e *engine) post(cb leafCB) {
	select {
	case e.cbs <- cb:
	case <-e.loopDone:
	}
}

// drive is the engine loop: the calling goroutine OWNS the goja Runtime, executing the
// script's continuations (the js halves) and every engine-state mutation (the state
// halves) single-threaded — the engine's one serialization point. It
// returns once the script's promise has settled AND no leaf is in flight:
//
//	fulfilled → (value, nil)
//	rejected  → the rejection as the run error; in-flight leaves are cancelled and
//	            drained state-only (the run's outcome is already decided)
//	stopped   → "run stopped" once cancellation has drained the in-flight leaves;
//	            the caller finalizes the manifest as stopped, not failed
//	stuck     → errNoProgress
//
// A leaf promise that rejected and was never handled anywhere fails the otherwise
// fulfilled run — a leaf failure the script silently dropped is still a failure
// (handled-set semantics: a rejection awaited later is removed before this check).
func (e *engine) drive(prom *goja.Promise) (goja.Value, error) {
	for {
		settled := prom.State() != goja.PromiseStatePending
		if settled && e.inflight == 0 {
			break
		}
		if e.aborted {
			if e.inflight == 0 {
				break
			}
			cb := <-e.cbs
			cb.state()
			continue
		}
		if settled && prom.State() == goja.PromiseStateRejected {
			// The script failed while unawaited leaves are still in flight: the run's
			// outcome is decided, so stop them rather than spend into a failed run.
			e.abort()
			continue
		}
		if e.inflight == 0 {
			select {
			case cb := <-e.cbs:
				e.runCB(cb)
			default:
				// With zero leaves the loop never blocks on the ctx.Done select
				// below, so this is the ONLY point a stop of a leafless pending
				// script (e.g. `await new Promise(() => {})` then cancelled) gets
				// classified — and the backstop for any settle stranded outside
				// noteSettleErr's reach. The cancelled ctx decides "stopped" here
				// exactly as at the loop exit; on a live ctx a quiescent pending
				// promise remains a proven deadlock.
				if e.stopped || e.runCtx.Err() != nil {
					e.stopped = true
					return nil, errRunStopped
				}
				return nil, errNoProgress
			}
			continue
		}
		select {
		case cb := <-e.cbs:
			e.runCB(cb)
		case <-e.runCtx.Done():
			e.stopped = true
			e.abort()
		}
	}
	// A cancelled run ctx decides "stopped" regardless of which select branch observed
	// it first — a leaf completion racing the cancel may settle the script's promise
	// (with its stopped-class rejection) before the loop sees ctx.Done().
	if e.stopped || e.runCtx.Err() != nil {
		e.stopped = true
		return nil, errRunStopped
	}
	// A severed settlement on a live ctx (e.g. a stack overflow tripping the VM
	// mid-cascade) left the script unrecoverable, possibly with the promise still
	// pending — the recorded cause is the run error, never a nil result.
	if e.settleErr != nil {
		return nil, e.settleErr
	}
	if prom.State() == goja.PromiseStateRejected {
		return nil, rejectionError(prom.Result())
	}
	for _, reason := range e.unhandled {
		return nil, fmt.Errorf("workflow: unhandled promise rejection: %w", rejectionError(reason))
	}
	return prom.Result(), nil
}

// abort flips the loop into state-only draining, releases every HELD leaf (a held leaf
// has no goroutine to post a completion — without the release the drain would wait on
// inflight forever), and cancels every in-flight attempt's exec ctx; the Interrupt
// watchdog (run) separately kills any JS busy mid-callback.
func (e *engine) abort() {
	e.aborted = true
	e.releaseHeld()
	e.cancelLeaves()
}

// noteSettleErr handles the error goja's promise resolve/reject return (their
// documented propagate-upwards contract). A non-nil error means an uncatchable — an
// Interrupt, a stack overflow — tripped the settle's reaction drain and goja dropped
// the queued jobs: the script's await graph is unrecoverable, so the run aborts with
// the cause in hand instead of stranding into a false deadlock at quiescence.
// Loop-held: settles run only in js halves, and abort's callees touch no JS.
func (e *engine) noteSettleErr(err error) {
	if err == nil {
		return
	}
	if e.settleErr == nil {
		e.settleErr = fmt.Errorf("workflow: promise settlement interrupted: %w", err)
	}
	if e.runCtx.Err() != nil {
		e.stopped = true
	}
	e.abort()
}

// drainLeaves cancels and consumes every in-flight leaf state-only — the abnormal-exit
// twin of drive's aborted drain, for paths where the script promise never materialized
// (an Interrupt mid-body, a body that produced no promise). Without it a leaf spawned
// before the abort would strand its job file as `queued` forever.
func (e *engine) drainLeaves() {
	if e.inflight == 0 {
		return
	}
	e.abort()
	for e.inflight > 0 {
		cb := <-e.cbs
		cb.state()
	}
}

func (e *engine) runCB(cb leafCB) {
	cb.state()
	if !e.aborted && cb.js != nil {
		cb.js()
	}
}

// rejectionError renders a JS rejection value as a Go error: an Error object reports
// name + message; anything else stringifies.
func rejectionError(v goja.Value) error {
	if v == nil {
		return errors.New("rejected")
	}
	if o, ok := v.(*goja.Object); ok {
		if msg := o.Get("message"); msg != nil && !goja.IsUndefined(msg) {
			if name := o.Get("name"); name != nil && !goja.IsUndefined(name) {
				return fmt.Errorf("%s: %s", name.String(), msg.String())
			}
			return errors.New(msg.String())
		}
	}
	return errors.New(v.String())
}
