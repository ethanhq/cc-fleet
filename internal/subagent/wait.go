package subagent

import (
	"context"
	"time"
)

// waitJobDefaultInterval is WaitForJob's poll cadence; waitJobMinInterval floors a tiny
// POSITIVE interval so it can't busy-spin. <=0 still means "use the default".
const (
	waitJobDefaultInterval = 2 * time.Second
	waitJobMinInterval     = 100 * time.Millisecond
)

// WaitForJob polls StatusFor until the job settles — terminal (done | failed |
// stopped), held (operator-parked: waiting it out would hang forever), or a
// front-loaded failure envelope (unknown id / bad args carry no Status) — or until ctx
// ends. settled reports whether res is that final answer; false means ctx expired and
// res is the last nonterminal snapshot (a heartbeat). The poll inherits StatusFor's
// collapse of process-gone-without-result into a failed terminal cache, so a vanished
// job settles instead of hanging.
func WaitForJob(ctx context.Context, jobID string, interval time.Duration) (res Result, settled bool) {
	switch {
	case interval <= 0:
		interval = waitJobDefaultInterval
	case interval < waitJobMinInterval:
		interval = waitJobMinInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		res = StatusFor(jobID)
		if jobSettled(res) {
			return res, true
		}
		select {
		case <-ctx.Done():
			// One re-read so a job that settled while we slept isn't reported as
			// still pending.
			res = StatusFor(jobID)
			return res, jobSettled(res)
		case <-tick.C:
		}
	}
}

// jobSettled reports that res needs no further polling: anything but the two
// in-flight statuses (queued / running).
func jobSettled(res Result) bool {
	return res.Status != "running" && res.Status != "queued"
}
