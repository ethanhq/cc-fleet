package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// pinIdentityProfile pins resolveProfile to identity for the test, so the default-slim
// leaf shape never depends on the host claude version.
func pinIdentityProfile(t *testing.T) {
	t.Helper()
	old := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = old })
}

// TestPoolBoundsConcurrentExecs: concurrent vendor EXECS never exceed the pool, even with
// many elements — the slot (acquired in each leaf goroutine, held only across its exec) is
// the meaningful, deadlock-free bound on real vendor processes; a full pool queues leaf
// goroutines without ever stalling the script loop.
func TestPoolBoundsConcurrentExecs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinIdentityProfile(t)
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
	eng := newTestEngine(context.Background(), "pb", pool)
	done := make(chan struct{})
	go func() {
		_, _ = eng.run("pb.js", []byte(`
const thunks = [];
for (let i = 0; i < 30; i++) thunks.push(() => agent("x", {vendor: "v"}));
await parallel(thunks);
return {};
`), Options{})
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
	pinIdentityProfile(t)
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	eng := newTestEngine(context.Background(), "nd", 1)
	done := make(chan struct{})
	go func() {
		_, _ = eng.run("nd.js", []byte(`
await parallel([
    () => parallel([() => agent("a", {vendor: "v"}), () => agent("b", {vendor: "v"})]),
    () => parallel([() => agent("c", {vendor: "v"}), () => agent("d", {vendor: "v"})]),
]);
return {};
`), Options{})
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

// TestAcceptsLargeList: a parallel far larger than the pool RUNS (excess queues) rather
// than erroring; every element gets a result (the lifetime cap turns over-cap agents
// into null).
func TestAcceptsLargeList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	pinIdentityProfile(t)
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	// Skip the queued-placeholder disk writes — 1500 leaves would mint 1000 job files.
	oldMint := mintQueuedLeaf
	mintQueuedLeaf = func(subagent.Request, string) string { return "" }
	t.Cleanup(func() { mintQueuedLeaf = oldMint })

	eng := newTestEngine(context.Background(), "ll", 8)
	v, err := eng.run("ll.js", []byte(`
const thunks = [];
for (let i = 0; i < 1500; i++) thunks.push(() => agent("x", {vendor: "v"}));
const r = await parallel(thunks);
return { r };
`), Options{})
	if err != nil {
		t.Fatalf("a 1500-element parallel must run, not error: %v", err)
	}
	if got := listField(t, wantMap(t, v), "r"); len(got) != 1500 {
		t.Errorf("got %d results, want 1500", len(got))
	}
	if n := len(rec.prompts()); n > maxLifetimeAgents {
		t.Errorf("leaf execs = %d, must not exceed the %d lifetime cap", n, maxLifetimeAgents)
	}
}
