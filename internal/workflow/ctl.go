package workflow

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// ctlPollInterval is how often the engine polls runs/<id>.ctl for leaf directives.
const ctlPollInterval = 250 * time.Millisecond

// ctlCommand is one NDJSON line of the per-run control plane: a leaf-scoped directive a
// CLI/board writer appends and the engine's poller applies. Op + ids only — no prompts,
// no keys.
type ctlCommand struct {
	Op    string `json:"op"`              // stop | restart | stop-phase | restart-phase
	Leaf  string `json:"leaf,omitempty"`  // target job id (leaf ops)
	Phase string `json:"phase,omitempty"` // target phase title, "" included (phase ops)
}

// SendLeafCommand appends a leaf directive to a LIVE run's control file. It validates
// both ids (they become path components / file content) and requires the manifest to be
// `running` — a dead run's control file has no poller, so the append would silently rot.
func SendLeafCommand(runID, op, leafID string) error {
	if op != "stop" && op != "restart" {
		return fmt.Errorf("workflow: unknown leaf op %q", op)
	}
	if err := subagent.ValidateRunID(runID); err != nil {
		return fmt.Errorf("workflow: invalid run id: %w", err)
	}
	if err := ids.ValidateJobID(leafID); err != nil {
		return fmt.Errorf("workflow: invalid leaf id: %w", err)
	}
	run, err := subagent.ReadRun(runID)
	if err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	if run.Status != "running" {
		return fmt.Errorf("workflow: run %s is not running (leaf control needs a live engine; use the keyed restart for a finished run)", runID)
	}
	path, err := subagent.RunCtlPath(runID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ctlCommand{Op: op, Leaf: leafID})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, werr := f.Write(append(data, '\n'))
	return werr
}

// SendPhaseCommand appends a phase directive to a LIVE run's control file: the leaf
// atom fanned out over every member of the EXACT title (a reused title is one merged
// target; "" addresses unphased leaves), with future members of a held phase parking
// before their first exec.
func SendPhaseCommand(runID, op, phase string) error {
	if op != "stop" && op != "restart" {
		return fmt.Errorf("workflow: unknown phase op %q", op)
	}
	if err := subagent.ValidateRunID(runID); err != nil {
		return fmt.Errorf("workflow: invalid run id: %w", err)
	}
	run, err := subagent.ReadRun(runID)
	if err != nil {
		return fmt.Errorf("workflow: %w", err)
	}
	if run.Status != "running" {
		return fmt.Errorf("workflow: run %s is not running (phase control needs a live engine; use the keyed phase restart for a finished run)", runID)
	}
	path, err := subagent.RunCtlPath(runID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(ctlCommand{Op: op + "-phase", Phase: phase})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, werr := f.Write(append(data, '\n'))
	return werr
}

// startCtlPoller launches the control-plane reader: a goroutine tailing runs/<id>.ctl
// from a remembered offset (torn-tail tolerant — a partial last line is retried next
// tick) and posting each complete command onto the loop. It starts AFTER Execute
// truncated the file and stamped the manifest running, and dies with the loop.
func (e *engine) startCtlPoller(path string) {
	go func() {
		ticker := time.NewTicker(ctlPollInterval)
		defer ticker.Stop()
		var offset int64
		for {
			select {
			case <-e.loopDone:
				return
			case <-ticker.C:
			}
			f, err := os.Open(path)
			if err != nil {
				continue // not created yet (no writer) — keep polling
			}
			if _, err := f.Seek(offset, 0); err != nil {
				f.Close()
				continue
			}
			r := bufio.NewReader(f)
			for {
				line, rerr := r.ReadBytes('\n')
				if rerr != nil {
					break // EOF mid-line: the torn tail is re-read next tick from offset
				}
				offset += int64(len(line))
				var cmd ctlCommand
				if json.Unmarshal(line, &cmd) != nil {
					continue // a malformed line is dropped; the offset already moved past it
				}
				if cmd.Op == "stop-phase" || cmd.Op == "restart-phase" {
					e.post(leafCB{state: func() { e.applyPhaseDirective(cmd) }})
				} else {
					e.post(leafCB{state: func() { e.applyDirective(cmd) }})
				}
			}
			f.Close()
		}
	}()
}

// leafCtl is one leaf's loop-owned control state: the directive machine, linearized as
// loop-state transitions (commands and completions serialize on the loop, so there are
// no gap windows).
type leafCtl struct {
	spec     leafSpec
	settle   promiseSettle
	gen      int    // attempt ordinal (== Request.Attempt)
	pending  string // "" | "stop" | "restart" — armed, consumed by the attempt's completion
	held     bool
	spawned  bool // at least one attempt goroutine ran (false = parked by a held phase)
	released bool // the frame's budget reservation was released (exactly once)
	cancel   func()
}

// applyDirective is the poller's loop callback: the command side of the directive
// machine. Idempotent no-ops and stale/unknown targets narrate instead of erroring —
// the control file is fire-and-forget and effects surface on the board.
func (e *engine) applyDirective(cmd ctlCommand) {
	if e.aborted {
		return
	}
	h := e.ctl[cmd.Leaf]
	if h == nil {
		e.logf("control: leaf %s is not controllable (already finished or unknown); directive %q dropped", cmd.Leaf, cmd.Op)
		return
	}
	switch cmd.Op {
	case "stop":
		e.stopLeafAtom(cmd.Leaf, h)
	case "restart":
		switch {
		case h.held:
			e.wakeLeaf(cmd.Leaf, h)
		case h.pending == "stop":
			h.pending = "restart" // upgrade: never strand a hold the user already retried
		case h.pending == "restart":
			e.logf("control: leaf %s already restarting; duplicate dropped", cmd.Leaf)
		default:
			// Queued or running alike: cancel the attempt and retry. Whether the slot
			// was already acquired is unknowable race-free from here, and dropping a
			// user's restart is worse than re-queuing a not-yet-started attempt.
			e.restartLeafAtom(cmd.Leaf, h)
		}
	}
}

// applyPhaseDirective fans the leaf atom out over the EXACT title's live members and,
// for a stop, parks the phase so future leaves minted into it hold before their first
// exec. done/failed members are untouched (their values are consumed).
func (e *engine) applyPhaseDirective(cmd ctlCommand) {
	if e.aborted {
		return
	}
	switch cmd.Op {
	case "stop-phase":
		e.heldPhases[cmd.Phase] = true
		for jobID, h := range e.ctl {
			if h.spec.phase != cmd.Phase {
				continue
			}
			e.stopLeafAtom(jobID, h)
		}
	case "restart-phase":
		delete(e.heldPhases, cmd.Phase)
		for jobID, h := range e.ctl {
			if h.spec.phase != cmd.Phase {
				continue
			}
			switch {
			case h.held:
				e.wakeLeaf(jobID, h)
			case h.pending == "stop":
				h.pending = "restart"
			case h.pending == "":
				e.restartLeafAtom(jobID, h)
			}
		}
	}
}

// stopLeafAtom arms the kill-and-HOLD directive on one live leaf: pre-mark the meta
// held BEFORE the kill (the killed attempt's stopped-class finalize is suppressed, so
// no terminal cache can exist in the kill window for GC to misread), then cancel the
// attempt. Idempotent on held / already-stopping leaves; overrides a pending restart
// (the user's latest intent is to halt).
func (e *engine) stopLeafAtom(jobID string, h *leafCtl) {
	if h.held || h.pending == "stop" {
		return
	}
	h.pending = "stop"
	subagent.HoldLeaf(jobID)
	if h.cancel != nil {
		h.cancel()
	}
}

// restartLeafAtom arms kill-and-retry on one running leaf (the same suppression window
// as a stop; the completion respawns instead of holding).
func (e *engine) restartLeafAtom(jobID string, h *leafCtl) {
	h.pending = "restart"
	subagent.HoldLeaf(jobID)
	if h.cancel != nil {
		h.cancel()
	}
}

// wakeLeaf re-runs a held leaf in place: re-gate the caps (the frame's reservation is
// already counted in `reserved`, so the gate checks exhaustion only), requeue the SAME
// job at the next attempt, and spawn a fresh attempt goroutine whose completion
// eventually settles the SAME promise — the script observes one slow leaf.
func (e *engine) wakeLeaf(jobID string, h *leafCtl) {
	if e.budgetWouldExceed(0, 0) {
		e.logf("control: leaf %s restart refused — %v; the leaf stays held", jobID, e.budgetExceededErr())
		return
	}
	// A leaf parked by a held PHASE never ran its first attempt: waking it is that
	// first spawn (already admitted at agent()), not a retry.
	if h.spawned {
		if !e.sched.admit() {
			e.logf("control: leaf %s restart refused — the %d-leaf lifetime cap is exhausted; the leaf stays held", jobID, maxLifetimeAgents)
			return
		}
		h.gen++
	}
	h.held = false
	h.pending = ""
	subagent.RequeueLeaf(jobID, h.gen)
	e.spawnAttempt(jobID, h)
}

// releaseHeld terminal-stops every held leaf at run abort: a held leaf has no goroutine
// to post a completion, so without this the aborted drain would wait on inflight
// forever. The stopped-class finalize re-stamps the terminal cache HoldLeaf suppressed
// (the resume re-runs the leaf; holds are never persisted).
func (e *engine) releaseHeld() {
	for jobID, h := range e.ctl {
		if !h.held {
			continue
		}
		subagent.ReleaseHeldLeafStopped(jobID, "run stopped while the leaf was held")
		e.emitLeaf("stopped", h.spec.phase, h.spec.label, h.spec.provider, h.spec.model)
		e.releaseLeaf(jobID, h)
	}
}

// releaseLeaf retires a leaf's control handle and frame accounting exactly once: the
// reservation release is flag-guarded so no path can double-free it.
func (e *engine) releaseLeaf(jobID string, h *leafCtl) {
	if !h.released {
		h.released = true
		e.budgetRelease(h.spec.usdEst, h.spec.tokEst)
	}
	e.inflight--
	delete(e.ctl, jobID)
}
