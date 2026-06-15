package subagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Cross-platform background-job board tests. The exec/reap-driven cases (fake
// claude via /bin/sh, syscall.Wait4) live in job_unix_test.go.

// TestRemoveJob_PreservesScanLock: removeJob GCs a job's data sidecars but must never unlink the
// .scan.lock flock — a concurrent board poll may still hold it, and unlink+recreate would hand a new
// scanner a different inode and break the live-scan serialization (mirrors the per-run .lock rule).
func TestRemoveJob_PreservesScanLock(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-1111-1111-1111-111111111111"
	for _, suffix := range []string{".json", ".out", ".scan", ".scan.lock"} {
		if err := os.WriteFile(filepath.Join(dir, id+suffix), nil, 0o600); err != nil {
			t.Fatalf("seed %s: %v", suffix, err)
		}
	}
	removeJob(dir, id)
	if _, err := os.Stat(filepath.Join(dir, id+".scan.lock")); err != nil {
		t.Fatalf(".scan.lock must survive removeJob, got %v", err)
	}
	for _, suffix := range []string{".json", ".out", ".scan"} {
		if _, err := os.Stat(filepath.Join(dir, id+suffix)); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed, got %v", suffix, err)
		}
	}
}

func TestStatusFor_RunningJobStaysRunning(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "job-running"
	// Use the test process's own pid as a guaranteed-alive process.
	if err := writeMeta(dir, jobMeta{
		JobID: jobID, PID: os.Getpid(), Provider: "glm", Model: "glm-4.6",
		StartedAt: time.Now().UTC().Format(time.RFC3339), Status: "running", JSON: true,
	}); err != nil {
		t.Fatal(err)
	}
	if st := StatusFor(jobID); st.Status != "running" || !st.OK {
		t.Fatalf("alive job should be running: %+v", st)
	}
}

func TestListJobs_EmptyDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // jobs dir won't exist
	t.Setenv("HOME", t.TempDir())
	jobs, err := ListJobs()
	if err != nil {
		t.Fatalf("ListJobs empty dir: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("want 0 jobs from a missing dir, got %d", len(jobs))
	}
}
