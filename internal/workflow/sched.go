package workflow

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"

	"go.starlark.net/starlark"
)

// maxLifetimeAgents is the absolute ceiling on agent() leaves one run may spawn —
// a runaway-loop backstop, the same bound the native runtime uses.
const maxLifetimeAgents = 1000

// maxConcurrencyCap mirrors the native scheduler's hard upper bound on concurrent
// leaves; the effective default is min(this, cores-2), floored at 1.
const maxConcurrencyCap = 16

// maxFanoutElements bounds a single parallel/pipeline list — far above the lifetime cap
// so a large list (the native "excess queues" case) is accepted, but finite so a
// pathological list can't OOM the results slice. Live goroutines stay ~pool size
// regardless (fanout acquires a slot before spawning each element).
const maxFanoutElements = 100_000

// maxThreadSteps caps Starlark bytecode steps PER THREAD (top-level and every
// parallel/pipeline goroutine thread), so a pure-CPU runaway in script glue is
// bounded even though `while` is disabled. Generous: real orchestration glue is
// tiny; the work is in the (separately timeout-bounded) vendor leaves.
const maxThreadSteps = 1 << 32

// scheduler is a run's shared concurrency core. The GIL serializes ALL Starlark
// interpreter execution (at most one goroutine runs bytecode at a time), which is
// what makes the engine -race clean; the bounded slot pool throttles concurrent
// vendor execs (the slow part, which runs with the GIL RELEASED so it overlaps);
// the lifetime counter is the runaway backstop.
type scheduler struct {
	gil     sync.Mutex
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

func (s *scheduler) lock()   { s.gil.Lock() }
func (s *scheduler) unlock() { s.gil.Unlock() }

// runBlocking releases the GIL, runs fn (the blocking vendor exec / slot wait), and
// re-acquires the GIL — even if fn PANICS (defer s.lock()). That defer is the
// load-bearing invariant of the whole engine: every builtin is entered with the GIL
// held and leaves it held on BOTH normal return and panic-unwind, so the goroutine
// wrappers (which lock before starlark.Call and unlock in a defer) can always unlock
// exactly once without risking "unlock of unlocked mutex".
func (s *scheduler) runBlocking(fn func()) {
	s.unlock()
	defer s.lock()
	fn()
}

// acquireSlot blocks for a pool slot, honoring engine cancellation. Returns false if
// the engine ctx is cancelled before a slot is obtained. The upfront ctx check means
// that once the run is cancelled no NEW leaf launches (even if a slot is momentarily
// free); a leaf already in flight is unaffected (bounded by its own timeout). MUST be
// called with the GIL released (via runBlocking) so a goroutine waiting for a slot
// never pins the interpreter.
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

// admit reserves one unit of the lifetime budget; false once the cap is reached. It
// is called under the GIL so the count is exact. The budget is a monotonic ceiling
// counted at admission and never refunded (a cancelled-before-launch agent still
// counts — harmless for an aborting run).
func (s *scheduler) admit() bool { return s.spawned.Add(1) <= maxLifetimeAgents }

// newThread builds a fresh *starlark.Thread for the top level or a goroutine, with the
// per-thread CPU backstop set (SetMaxExecutionSteps → the default OnMaxSteps cancels a
// runaway). Every goroutine MUST use its OWN thread — reusing the top-level thread on a
// goroutine would corrupt its per-thread call stack.
//
// Engine cancellation (SIGINT/SIGTERM via the run ctx) is honored at slot admission
// (acquireSlot stops launching NEW leaves) and lets the run finalize its manifest; it
// does NOT interrupt Starlark bytecode already running on a thread — the step cap above
// is the CPU backstop, and an in-flight leaf is bounded by its own per-agent timeout.
// Interrupting in-flight bytecode via Thread.Cancel rides with the v4 live-stop command.
func (s *scheduler) newThread(name string) *starlark.Thread {
	th := &starlark.Thread{Name: name}
	th.SetMaxExecutionSteps(maxThreadSteps)
	return th
}
