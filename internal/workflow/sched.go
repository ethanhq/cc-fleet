package workflow

import (
	"context"
	"runtime"
	"sync/atomic"
)

// maxLifetimeAgents is the absolute ceiling on agent() leaves one run may spawn —
// a runaway-loop backstop, the same bound the native runtime uses.
const maxLifetimeAgents = 1000

// maxConcurrencyCap mirrors the native scheduler's hard upper bound on concurrent
// leaves; the effective default is min(this, cores-2), floored at 1.
const maxConcurrencyCap = 16

// maxFanoutElements bounds a single parallel/pipeline list — far above the lifetime cap
// so a large list (the native "excess queues" case) is accepted, but finite so a
// pathological list can't OOM the results array. Live vendor execs stay ~pool size
// regardless (each leaf goroutine waits for a slot before its exec).
const maxFanoutElements = 100_000

// scheduler is a run's shared concurrency core: the bounded slot pool throttles
// concurrent vendor execs (each leaf goroutine acquires a slot around its own exec —
// never the loop, so a full pool queues leaves without stalling the script), and the
// lifetime counter is the runaway backstop. Script execution itself is serialized by
// the engine loop (loop.go).
type scheduler struct {
	slots   chan struct{}
	ctx     context.Context
	spawned atomic.Int64
}

func newScheduler(ctx context.Context, concurrency int) *scheduler {
	if concurrency < 1 {
		concurrency = 1
	}
	return &scheduler{slots: make(chan struct{}, concurrency), ctx: ctx}
}

// defaultConcurrency is max(1, min(16, cores-2)) — floored so 1–2-core hosts can't
// deadlock on a zero-width pool, capped like the native runtime.
func defaultConcurrency() int {
	n := runtime.NumCPU() - 2
	if n > maxConcurrencyCap {
		n = maxConcurrencyCap
	}
	if n < 1 {
		n = 1
	}
	return n
}

// acquireSlot blocks for a pool slot, honoring run cancellation. Returns false if the
// leaf ctx is cancelled before a slot is obtained. The upfront ctx check means that
// once the run is aborting no NEW leaf launches (even if a slot is momentarily free);
// a leaf already in flight is bounded by its own exec ctx. Called from leaf goroutines
// only — never the loop, so a full pool can never stall script execution.
func (s *scheduler) acquireSlot() bool {
	if s.ctx.Err() != nil {
		return false
	}
	select {
	case s.slots <- struct{}{}:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *scheduler) releaseSlot() { <-s.slots }

// admit reserves one unit of the lifetime budget; false once the cap is reached. It is
// called on the loop, so the count is exact. The budget is a monotonic ceiling counted
// at admission and never refunded (a cancelled-before-launch agent still counts —
// harmless for an aborting run).
func (s *scheduler) admit() bool { return s.spawned.Add(1) <= maxLifetimeAgents }
