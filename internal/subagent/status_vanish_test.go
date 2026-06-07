package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// deadPID returns a pid that has exited but is not yet recycled within the test window.
func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("true")
	if err := c.Start(); err != nil {
		t.Skipf("cannot spawn a throwaway process: %v", err)
	}
	_ = c.Wait()
	return c.Process.Pid
}

// A job with no recorded process (PID<=0) and no cached terminal result has produced no terminal
// signal, so StatusFor reports it queued regardless of the Status string — never a done leaf. The
// dead-classify path below it would otherwise read a queued placeholder's empty .out (the
// placeholder is minted in text mode) plus the synthetic exit 0 as a "done · 0 turns" success.
func TestStatusForNoProcessReportsQueuedNotDone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := jobMeta{JobID: "joba", PID: 0, Status: "running", JSON: false,
		Vendor: "v", Model: "mm", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if st := StatusFor("joba"); st.Status != "queued" || !st.OK {
		t.Fatalf("PID=0 no-output job: status=%q ok=%v, want queued/ok (never done/failed)", st.Status, st.OK)
	}
}

// A dead detached leaf with an empty .out fails with an honest message — never the cause-erasing
// "claude exited 0 without a result envelope". A Released detached job has no wait()-able exit code,
// so StatusFor fabricates exitCode 0; the message must say the exit status is unknown, not assert a
// clean "exited 0".
func TestStatusForVanishedLeafHonestFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 0 // no re-read wait for a genuinely-empty capture
	pid := deadPID(t)
	m := jobMeta{JobID: "jobb", PID: pid, PGID: pid, Status: "running", JSON: true,
		SettingsPath: "/no/such/profile", Vendor: "v", Model: "mm",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "jobb.out"), nil, 0o600)
	st := StatusFor("jobb")
	if st.Status != "failed" {
		t.Fatalf("dead bg leaf with empty .out: status=%q, want failed", st.Status)
	}
	if strings.Contains(st.ErrorMsg, "exited 0") {
		t.Fatalf("cause-erasing message %q (no real exit code exists for a detached job)", st.ErrorMsg)
	}
}

// A still-running leaf reaped by `workflow stop` (finalizeRunLeaves) reports a neutral terminal
// "stopped" — not "failed" — so the board distinguishes a deliberate stop from a real failure.
func TestFinalizeRunLeavesMarksStopped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := jobMeta{JobID: "s1", PID: os.Getpid(), Status: "running", RunID: "runX",
		Vendor: "v", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	finalizeRunLeaves("runX")
	st := StatusFor("s1")
	if st.Status != "stopped" {
		t.Fatalf("stopped leaf status = %q, want stopped", st.Status)
	}
	if st.OK {
		t.Errorf("a stopped leaf must have OK=false")
	}
}

// A detached bg leaf seen dead with an as-yet-unwritten envelope is recovered by the confirm-delay
// re-read, not cached as a spurious failure. A goroutine writes the envelope just after StatusFor's
// first (empty) read, so only the re-read sees it.
func TestStatusForConfirmDelayRecoversLateEnvelope(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 100 * time.Millisecond
	pid := deadPID(t)
	m := jobMeta{JobID: "jobc", PID: pid, PGID: pid, Status: "running", JSON: true,
		SettingsPath: "/no/such/profile", Vendor: "v", Model: "mm",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "jobc.out")
	_ = os.WriteFile(outPath, nil, 0o600)
	envelope := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"late answer","num_turns":1,"total_cost_usd":0.001}`)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.WriteFile(outPath, envelope, 0o600)
	}()
	st := StatusFor("jobc")
	if st.Status != "done" {
		t.Fatalf("late-envelope bg leaf: status=%q, want done (confirm-delay re-read should recover it)", st.Status)
	}
	if st.Result != "late answer" {
		t.Errorf("recovered result = %q, want \"late answer\"", st.Result)
	}
}
