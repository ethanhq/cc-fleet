package subagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Cross-platform background-job board tests. The exec/reap-driven cases (fake
// claude via /bin/sh, syscall.Wait4) live in job_unix_test.go.

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
		JobID: jobID, PID: os.Getpid(), Vendor: "glm", Model: "glm-4.6",
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
