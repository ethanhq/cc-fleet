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

// StatusFor reports a PID<=0 job with no cached result as queued, regardless of meta.Status — never done.
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
		Provider: "v", Model: "mm", StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if st := StatusFor("joba"); st.Status != "queued" || !st.OK {
		t.Fatalf("PID=0 no-output job: status=%q ok=%v, want queued/ok (never done/failed)", st.Status, st.OK)
	}
}

// A dead detached leaf with an empty .out fails with an honest unknown-exit message, never "exited 0".
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
		SettingsPath: "/no/such/profile", Provider: "v", Model: "mm",
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
		Provider: "v", StartedAt: time.Now().UTC().Format(time.RFC3339)}
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

// The confirm-delay re-read recovers a late-landing envelope instead of caching a failure (a goroutine
// writes it just after the first empty read).
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
		SettingsPath: "/no/such/profile", Provider: "v", Model: "mm",
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

// A dead legacy (non-stream) json bg leaf whose .out exceeds the cap fails honestly, even when its
// under-cap prefix is a VALID result envelope: an over-cap capture is parsed as vanished, never a
// truncated prefix trusted as done (which would fabricate a result the run never authoritatively wrote).
func TestStatusFor_OverCapLegacyOutFailsNotFabricated(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 0
	origCap := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = origCap })

	pid := deadPID(t)
	m := jobMeta{JobID: "jobcap", PID: pid, PGID: pid, Status: "running", JSON: true, // legacy: Stream=false
		SettingsPath: "/no/such/profile", Provider: "v", Model: "mm",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	// A valid envelope under the cap, then whitespace past the cap, then garbage: a truncating read
	// at the cap would strip to the clean envelope and fabricate done; the whole capture is over-cap.
	envelope := `{"type":"result","subtype":"success","is_error":false,"result":"the answer","num_turns":1,"total_cost_usd":0.001}`
	content := envelope + strings.Repeat(" ", maxChildOutput) + "GARBAGE-PAST-THE-CAP"
	if err := os.WriteFile(filepath.Join(dir, "jobcap.out"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	st := StatusFor("jobcap")
	if st.Status != "failed" || st.OK || st.ErrorCode != ErrCodeFailed {
		t.Fatalf("over-cap legacy .out must fail via failVanished, not fabricate done: %+v", st)
	}
	if st.Result == "the answer" {
		t.Fatalf("fabricated a result from a truncated over-cap prefix: %q", st.Result)
	}
}

// A dead stream-json bg leaf whose transcript has an oversized intermediate line before a success
// result classifies done — the oversized line no longer halts the terminal extract into a failure.
func TestStatusFor_OversizedStreamClassifiesDone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 0
	origCap := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = origCap })

	pid := deadPID(t)
	m := jobMeta{JobID: "jobov", PID: pid, PGID: pid, Status: "running", JSON: true, Stream: true,
		SettingsPath: "/no/such/profile", Provider: "v", Model: "mm",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxChildOutput*2) + `"}`
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		huge,
		`{"type":"result","subtype":"success","is_error":false,"result":"the answer","num_turns":1,"total_cost_usd":0.001}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "jobov.out"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	st := StatusFor("jobov")
	if !st.OK || st.Status != "done" || st.Result != "the answer" {
		t.Fatalf("oversized stream classify: %+v", st)
	}
	// The done result is cached (finalize-once).
	cached, err := os.ReadFile(filepath.Join(dir, "jobov.result.json"))
	if err != nil {
		t.Fatalf("terminal result not cached: %v", err)
	}
	if !strings.Contains(string(cached), "the answer") {
		t.Fatalf("cached result missing the answer: %s", cached)
	}
}

// A dead stream-json bg leaf whose transcript has an oversized line and NO result line still fails
// honestly via failVanished — the tolerant extract never fabricates a result.
func TestStatusFor_ResultlessStreamStillFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := jobsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	statusConfirmDelay = 0
	origCap := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = origCap })

	pid := deadPID(t)
	m := jobMeta{JobID: "jobrl", PID: pid, PGID: pid, Status: "running", JSON: true, Stream: true,
		SettingsPath: "/no/such/profile", Provider: "v", Model: "mm",
		StartedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := writeMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxChildOutput*2) + `"}`
	transcript := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		huge,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "jobrl.out"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	st := StatusFor("jobrl")
	if st.Status != "failed" || st.OK || st.ErrorCode != ErrCodeFailed {
		t.Fatalf("resultless stream must fail via failVanished: %+v", st)
	}
}
