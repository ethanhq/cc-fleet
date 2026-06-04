package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestPoolBoundsConcurrentExecs: concurrent vendor EXECS never exceed the pool, even with
// many elements — the slot (held only across a leaf's exec) is the meaningful, deadlock-free
// bound on real vendor processes.
func TestPoolBoundsConcurrentExecs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	release := make(chan struct{})
	started := make(chan struct{}, 4096)
	var live, maxLive int32
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		n := atomic.AddInt32(&live, 1)
		for {
			m := atomic.LoadInt32(&maxLive)
			if n <= m || atomic.CompareAndSwapInt32(&maxLive, m, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&live, -1)
		return subagent.Result{OK: true, Result: "ok"}
	})
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	const pool = 3
	eng := &engine{sched: newScheduler(context.Background(), pool), runID: "pb"}
	done := make(chan struct{})
	go func() {
		_, _ = eng.run("pb.star", `r = parallel([lambda: agent("x", vendor="v") for _ in range(30)])`, Options{})
		close(done)
	}()
	for i := 0; i < pool; i++ {
		<-started // the pool is full of blocked execs
	}
	if m := atomic.LoadInt32(&maxLive); m > pool {
		t.Errorf("concurrent execs = %d, exceeds pool %d", m, pool)
	}
	close(release)
	<-done
	if m := atomic.LoadInt32(&maxLive); m != pool {
		t.Errorf("peak concurrent execs = %d, want exactly the pool %d", m, pool)
	}
}

// TestNestedParallelNoDeadlock is the regression for the deadlock a branch-held slot permit
// would cause: a parallel whose BOTH branches themselves fan out, at pool=1 (the worst
// case), must COMPLETE — slots are held only during leaf execs, never across a branch's
// nested orchestration, so the inner leaves can always get the one slot.
func TestNestedParallelNoDeadlock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "nd"}
	done := make(chan struct{})
	go func() {
		_, _ = eng.run("nd.star", `
r = parallel([
    lambda: parallel([lambda: agent("a", vendor="v"), lambda: agent("b", vendor="v")]),
    lambda: parallel([lambda: agent("c", vendor="v"), lambda: agent("d", vendor="v")]),
])`, Options{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nested parallel deadlocked (a branch-held slot permit would hang here)")
	}
	if n := len(rec.prompts()); n != 4 {
		t.Errorf("nested leaves ran %d times, want 4", n)
	}
}

// TestAcceptsLargeList: a parallel larger than the old per-list cap RUNS (excess queues)
// rather than erroring; every element gets a result (the lifetime cap turns over-cap agents
// into None).
func TestAcceptsLargeList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 8), runID: "ll"}
	g, err := eng.run("ll.star", `r = parallel([lambda: agent("x", vendor="v") for _ in range(1500)])`, Options{})
	if err != nil {
		t.Fatalf("a 1500-element parallel must run, not error: %v", err)
	}
	if got := wantStringList(t, g, "r"); len(got) != 1500 {
		t.Errorf("got %d results, want 1500", len(got))
	}
	if n := len(rec.prompts()); n > maxLifetimeAgents {
		t.Errorf("leaf execs = %d, must not exceed the %d lifetime cap", n, maxLifetimeAgents)
	}
}
