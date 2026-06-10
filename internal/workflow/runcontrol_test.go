package workflow

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestStopRunUniformlyStopped (W-stop-run-uniform): cancelling a run mid-flight marks
// EVERY member leaf that exists `stopped` — the in-flight one (its exec ctx cancels),
// the queued ones (their slot wait cancels), and a leaf spawned just before the watchdog
// interrupted the script body alike; never a spurious `failed`, never a stranded
// `queued`. Member COUNT may legitimately fall below the script's three when the
// interrupt lands mid-spawn — uniformity is about the members that exist. Repeated to
// shake the cancel-vs-completion and interrupt-vs-spawn races.
func TestStopRunUniformlyStopped(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		var started atomic.Bool
		old := runLeaf
		runLeaf = func(ctx context.Context, req subagent.Request) subagent.Result {
			started.Store(true)
			<-ctx.Done() // the in-flight leaf dies only when its exec ctx cancels
			return subagent.Result{OK: false, ErrorCode: subagent.ErrCodeStopped, ErrorMsg: "stopped"}
		}
		t.Cleanup(func() { runLeaf = old })
		oldR := resolveProfile
		resolveProfile = func(requested string) (string, string) { return requested, "" }
		t.Cleanup(func() { resolveProfile = oldR })

		ctx, cancel := context.WithCancel(context.Background())
		eng := newTestEngine(ctx, "uni", 1) // pool of 1 → later leaves queue behind the first
		src := `return await parallel([() => agent("a", {provider: "v"}), () => agent("b", {provider: "v"}), () => agent("c", {provider: "v"})]);`

		done := make(chan error, 1)
		go func() {
			_, err := eng.run("u.js", []byte(src), Options{})
			done <- err
		}()
		for !started.Load() {
			time.Sleep(time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "stopped") {
				t.Fatalf("iter %d: err = %v, want run stopped", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: run did not return after cancel", i)
		}
		jobs, lerr := subagent.ListJobs()
		if lerr != nil {
			t.Fatalf("iter %d: list jobs: %v", i, lerr)
		}
		if len(jobs) == 0 {
			t.Fatalf("iter %d: no member jobs (the started leaf must have minted)", i)
		}
		for _, j := range jobs {
			if j.Status != "stopped" {
				t.Fatalf("iter %d: job %s status = %q, want stopped (uniform — never failed, never stranded)", i, j.JobID, j.Status)
			}
		}
	}
}
