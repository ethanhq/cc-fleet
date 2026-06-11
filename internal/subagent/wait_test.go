package subagent

import (
	"context"
	"os"
	"testing"
	"time"
)

// plantJobMeta writes a job meta file directly (the seam every status test uses).
func plantJobMeta(t *testing.T, m jobMeta) {
	t.Helper()
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if m.StartedAt == "" {
		m.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
}

// TestWaitForJobFlipsToDone: a running job that finalizes mid-wait settles with the
// cached terminal result.
func TestWaitForJobFlipsToDone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	plantJobMeta(t, jobMeta{JobID: "jw1", PID: os.Getpid(), Status: "running", Provider: "p", Model: "m"})
	go func() {
		time.Sleep(30 * time.Millisecond)
		finalizeSyncJob("jw1", Result{OK: true, Status: "done", JobID: "jw1", Provider: "p"})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, settled := WaitForJob(ctx, "jw1", waitJobMinInterval)
	if !settled || res.Status != "done" || !res.OK {
		t.Fatalf("settled=%v status=%q ok=%v, want settled done", settled, res.Status, res.OK)
	}
}

// TestWaitForJobHeldSettlesImmediately: held is operator-parked — WaitForJob must
// return it as settled rather than wait it out.
func TestWaitForJobHeldSettlesImmediately(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	plantJobMeta(t, jobMeta{JobID: "jw2", PID: 0, Status: "held", Provider: "p", Model: "m"})
	res, settled := WaitForJob(context.Background(), "jw2", waitJobMinInterval)
	if !settled || res.Status != "held" {
		t.Fatalf("settled=%v status=%q, want settled held", settled, res.Status)
	}
}

// TestWaitForJobHeartbeat: ctx expiring on a still-running job returns the nonterminal
// snapshot with settled=false (the heartbeat), after one final re-read.
func TestWaitForJobHeartbeat(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	plantJobMeta(t, jobMeta{JobID: "jw3", PID: os.Getpid(), Status: "running", Provider: "p", Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, settled := WaitForJob(ctx, "jw3", 5*time.Second) // one slow tick: ctx fires first
	if settled || res.Status != "running" {
		t.Fatalf("settled=%v status=%q, want a running heartbeat", settled, res.Status)
	}
}

// TestWaitForJobVanishedCollapses: a dead detached job with no result settles failed —
// StatusFor's vanished collapse is the wait loop's liveness backstop.
func TestWaitForJobVanishedCollapses(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	statusConfirmDelay = 0
	pid := deadPID(t)
	plantJobMeta(t, jobMeta{JobID: "jw4", PID: pid, PGID: pid, Status: "running", JSON: true,
		SettingsPath: "/no/such/profile", Provider: "p", Model: "m"})
	dir, _ := jobsDir()
	_ = os.WriteFile(dir+"/jw4.out", nil, 0o600)
	res, settled := WaitForJob(context.Background(), "jw4", waitJobMinInterval)
	if !settled || res.Status != "failed" {
		t.Fatalf("settled=%v status=%q, want settled failed (vanished collapse)", settled, res.Status)
	}
}

// TestWaitForJobUnknownSettles: an unknown id is a front-loaded failure envelope (no
// Status) — settled immediately, never polled.
func TestWaitForJobUnknownSettles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	res, settled := WaitForJob(context.Background(), "no-such-job", waitJobMinInterval)
	if !settled || res.OK {
		t.Fatalf("settled=%v ok=%v, want an immediate not-ok settle", settled, res.OK)
	}
}
