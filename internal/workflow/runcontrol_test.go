package workflow

import (
	"context"
	"errors"
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

// TestDriveClassifiesStoppedAtQuiescence (W-stop-quiescence): a cancelled run whose
// script promise is stranded pending reaches drive's quiescence branch — inflight 0,
// queue empty, promise pending — and must classify "stopped", never a false
// deadlock. The harness hands drive that exact quiescent state directly, so the
// misclassification would reproduce on every run, not once per thousand schedules.
func TestDriveClassifiesStoppedAtQuiescence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run ctx is already dead at quiescence
	eng := newTestEngine(ctx, "quiesce", 1)
	if err := eng.setupVM(); err != nil {
		t.Fatalf("setupVM: %v", err)
	}
	prom, _, _ := eng.vm.NewPromise() // pending; nothing will ever settle it

	_, err := eng.drive(prom)
	if err == nil || !strings.Contains(err.Error(), "run stopped") {
		t.Fatalf("err = %v, want run stopped (a cancelled ctx decides stopped at every exit)", err)
	}
	if !eng.stopped {
		t.Fatal("drive must set stopped so the finalizer stamps the manifest stopped")
	}
}

// TestDriveGenuineDeadlockStillDetected: the inverse pin — on a LIVE ctx a quiescent
// pending promise is a proven deadlock and keeps failing with errNoProgress.
func TestDriveGenuineDeadlockStillDetected(t *testing.T) {
	eng := newTestEngine(context.Background(), "deadlock", 1)
	if err := eng.setupVM(); err != nil {
		t.Fatalf("setupVM: %v", err)
	}
	prom, _, _ := eng.vm.NewPromise()

	_, err := eng.drive(prom)
	if !errors.Is(err, errNoProgress) {
		t.Fatalf("err = %v, want errNoProgress (genuine deadlock detection must survive)", err)
	}
}

// TestSeveredSettleAbortsWithCause (W-settle-sever): an uncatchable that fires inside
// a promise settle severs goja's reaction drain (the queued jobs are dropped and the
// interrupt is consumed) — the run must abort with that cause as the run error, not
// strand into the deadlock text. The interrupt is planted while the loop is provably
// parked in drive (a probe cb consumed on the loop proves the script body's JS is
// done), so the first JS after the flag is deterministically the settle's reaction
// job.
func TestSeveredSettleAbortsWithCause(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var started atomic.Bool
	gate := make(chan struct{})
	old := runLeaf
	runLeaf = func(ctx context.Context, req subagent.Request) subagent.Result {
		started.Store(true)
		<-gate
		return subagent.Result{OK: true, Result: "done"}
	}
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })

	eng := newTestEngine(context.Background(), "sever", 1) // live ctx: no cancel anywhere
	src := `return await parallel([() => agent("a", {provider: "v"})]);`
	done := make(chan error, 1)
	go func() {
		_, err := eng.run("s.js", []byte(src), Options{})
		done <- err
	}()
	for !started.Load() {
		time.Sleep(time.Millisecond)
	}
	// started only proves the leaf goroutine ran — the loop may still be finishing
	// the script body's JS, where the interrupt would exit via the callErr path
	// instead. A probe cb consumed on the loop proves drive is live.
	probe := make(chan struct{})
	eng.post(leafCB{state: func() { close(probe) }})
	<-probe
	eng.vm.Interrupt("severed") // sticky flag; trips on the settle's first reaction job
	close(gate)

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "settlement interrupted") {
			t.Fatalf("err = %v, want the severed-settlement cause as the run error", err)
		}
		if errors.Is(err, errNoProgress) {
			t.Fatal("a severed settlement must not report as a deadlock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the severed settle")
	}
}
