package workflow

import (
	"context"
	"fmt"
	"time"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// waitDefaultInterval is Wait's poll cadence (one manifest + jobs read per tick). It is
// deliberately slower than Watch's: there is no live stream to keep responsive, only an
// exit condition to notice. waitMinInterval floors a tiny POSITIVE --interval so it
// can't busy-spin; <=0 still means "use the default".
const (
	waitDefaultInterval = 2 * time.Second
	waitMinInterval     = 100 * time.Millisecond
)

// waitParkedTicks is the parked debounce: the predicate must hold on this many
// CONSECUTIVE polls before Wait declares the run parked, absorbing the hold-protocol
// transients (success-beats-kill clears a held leaf on the next engine tick; a restart
// flips held→queued, which breaks the predicate outright).
const waitParkedTicks = 4

// WaitOutcome classifies why Wait returned.
type WaitOutcome string

const (
	// WaitTerminal — the run reached done / failed / stopped.
	WaitTerminal WaitOutcome = "terminal"
	// WaitEngineGone — the manifest still says running but the detached engine is dead.
	WaitEngineGone WaitOutcome = "engine_gone"
	// WaitParked — every remaining leaf is held: the run cannot progress until an
	// operator restarts one (`workflow restart --leaf/--phase`) and waits indefinitely.
	WaitParked WaitOutcome = "parked"
	// WaitTimeout — ctx ended while the run was still progressing; the snapshot is a
	// heartbeat, not a final state.
	WaitTimeout WaitOutcome = "timeout"
)

// WaitCounts tallies a run's leaves by status at the moment Wait returned.
type WaitCounts struct {
	Running int `json:"running,omitempty"`
	Queued  int `json:"queued,omitempty"`
	Held    int `json:"held,omitempty"`
	Done    int `json:"done,omitempty"`
	Failed  int `json:"failed,omitempty"`
	Stopped int `json:"stopped,omitempty"`
}

// WaitHeldLeaf identifies one held leaf in a parked snapshot, so the caller can name
// what needs an operator without a second status call.
type WaitHeldLeaf struct {
	JobID string `json:"job_id"`
	Label string `json:"label,omitempty"`
}

// WaitResult is Wait's exit snapshot: the manifest as last read, why Wait returned, the
// leaf tally, and (parked) the held leaves.
type WaitResult struct {
	Run     subagent.WorkflowRun
	Outcome WaitOutcome
	Counts  WaitCounts
	Held    []WaitHeldLeaf
}

// WaitOptions configures a workflow-run wait.
type WaitOptions struct {
	// Interval is the poll cadence; <=0 uses waitDefaultInterval.
	Interval time.Duration
}

// Wait blocks until a run needs its caller's attention, polling the manifest + tagged
// jobs (never the events stream — every exit condition is a manifest/jobs fact). It is
// the quiet sibling of Watch, built to run in a backgrounded shell whose process exit
// IS the completion signal. INSPECTION-ONLY: the only writes are the dead-job result
// memoization any status read already performs.
//
// Returns nil with Outcome terminal or parked; ErrEngineGone with Outcome engine_gone;
// ctx.Err() with Outcome timeout (the snapshot is then a heartbeat, not a final state).
//
// Parked = 0 running ∧ 0 queued ∧ ≥1 held, stable for waitParkedTicks consecutive
// polls. The engine has no timers — leaf completion is its only async event source,
// and a pending script with zero in-flight leaves fails the run outright — so a
// running manifest whose only live leaves are held cannot progress until an operator
// restarts one. The check order (terminal → engine-gone → parked) needs no
// engine-alive conjunct on parked, so it fires for foreground runs (EnginePID 0) too.
// One window survives the debounce: a long synchronous JS continuation after a leaf
// settles, before the next agent() mints a job — the caller treats parked as
// "re-check, then act", not as a verdict.
func Wait(ctx context.Context, runID string, opts WaitOptions) (WaitResult, error) {
	interval := opts.Interval
	switch {
	case interval <= 0:
		interval = waitDefaultInterval
	case interval < waitMinInterval:
		interval = waitMinInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	parked := 0
	for {
		res, stop, err := waitPoll(runID, &parked)
		if stop {
			return res, err
		}
		select {
		case <-ctx.Done():
			// The run may have settled while we slept — one re-read before declaring a
			// heartbeat, so a just-finished run isn't misreported as a timeout.
			if res2, stop2, err2 := waitPoll(runID, &parked); stop2 {
				return res2, err2
			} else if err2 == nil {
				res = res2
			}
			res.Outcome = WaitTimeout
			return res, ctx.Err()
		case <-tick.C:
		}
	}
}

// waitPoll is one Wait tick: read the manifest + jobs, classify, advance the parked
// debounce. stop reports that res is a final answer (terminal / parked / engine-gone)
// or that the read failed (unknown id / IO — err set, snapshot empty).
func waitPoll(runID string, parkedTicks *int) (res WaitResult, stop bool, err error) {
	run, jobs, rerr := subagent.RunStatus(runID)
	if rerr != nil {
		return WaitResult{}, true, rerr
	}
	res = snapshotRun(run, jobs)
	switch {
	case isTerminalStatus(run.Status):
		res.Outcome = WaitTerminal
		return res, true, nil
	case run.EnginePID != 0 && !subagent.EngineAlive(run):
		res.Outcome = WaitEngineGone
		return res, true, ErrEngineGone
	}
	if res.Counts.Running == 0 && res.Counts.Queued == 0 && res.Counts.Held > 0 {
		*parkedTicks++
		if *parkedTicks >= waitParkedTicks {
			res.Outcome = WaitParked
			return res, true, nil
		}
	} else {
		*parkedTicks = 0
	}
	return res, false, nil
}

// snapshotRun tallies the run's leaves by status and collects the held ones.
func snapshotRun(run subagent.WorkflowRun, jobs []subagent.Result) WaitResult {
	res := WaitResult{Run: run}
	for _, j := range jobs {
		switch j.Status {
		case "running":
			res.Counts.Running++
		case "queued":
			res.Counts.Queued++
		case "held":
			res.Counts.Held++
			res.Held = append(res.Held, WaitHeldLeaf{JobID: j.JobID, Label: j.Label})
		case "done":
			res.Counts.Done++
		case "failed":
			res.Counts.Failed++
		case "stopped":
			res.Counts.Stopped++
		}
	}
	return res
}

// RenderWaitLine formats Wait's single human exit line: every opaque string is
// CleanTitle-scrubbed (terminal-injection defense) and it prints Status only — never
// the raw WorkflowRun.Error, which a schema-reject can taint (parity with finalLine).
func RenderWaitLine(res WaitResult) string {
	clean := sessiontitle.CleanTitle
	c := res.Counts
	s := fmt.Sprintf("run %s %s [%s] — running %d · queued %d · held %d · done %d · failed %d · stopped %d",
		clean(res.Run.RunID), clean(res.Run.Status), res.Outcome,
		c.Running, c.Queued, c.Held, c.Done, c.Failed, c.Stopped)
	switch {
	case res.Outcome == WaitParked:
		for _, h := range res.Held {
			if h.Label != "" {
				s += "\n  held: " + clean(h.Label) + " (" + clean(h.JobID) + ")"
			} else {
				s += "\n  held: " + clean(h.JobID)
			}
		}
		s += "\n  resume with `cc-fleet workflow restart " + clean(res.Run.RunID) + " --leaf <job|label>`"
	case res.Run.Status == "failed":
		s += " — run `cc-fleet workflow status " + clean(res.Run.RunID) + "` for the cause"
	}
	return s
}
