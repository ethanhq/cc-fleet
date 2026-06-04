//go:build !windows

package subagent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

func TestBackgroundLaunch(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	orig := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = orig })

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, Background: true, LeadSessionID: "lead-bg-1"})
	if !res.OK || res.JobID == "" || res.Status != "running" || res.PID <= 0 {
		t.Fatalf("background launch handle wrong: %+v", res)
	}
	if res.LeadSessionID != "lead-bg-1" {
		t.Fatalf("background launch should carry lead_session_id: %+v", res)
	}
	if res.OutputFile == "" {
		t.Fatalf("background launch missing output_file: %+v", res)
	}

	// The detached child should write the envelope to the .out file shortly.
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(res.OutputFile)
		if len(data) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatalf("detached child wrote nothing to %s", res.OutputFile)
	}

	// Reap the detached child so it doesn't linger as a zombie for the rest of
	// the test binary's life (in production cc-fleet exits and init reaps it).
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(res.PID, &ws, 0, nil)

	// Meta file must exist for subagent-status to find it.
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if _, err := os.Stat(filepath.Join(dir, res.JobID+".json")); err != nil {
		t.Fatalf("job meta not written: %v", err)
	}
	st := StatusFor(res.JobID)
	if st.LeadSessionID != "lead-bg-1" {
		t.Fatalf("background status should carry lead_session_id: %+v", st)
	}
}

func TestBackgroundLaunchAutoDetectsLeadSession(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	fakeClaude := writeFakeBin(t, "#!/bin/sh\nprintf '%s' '"+smokeSuccessJSON+"'\nexit 0\n")
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	origDetect := detectLeadSession
	detectLeadSession = func() string { return "auto-bg-session" }
	t.Cleanup(func() { detectLeadSession = origDetect })

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, Background: true})
	if !res.OK || res.JobID == "" {
		t.Fatalf("background launch failed: %+v", res)
	}
	if res.LeadSessionID != "auto-bg-session" {
		t.Fatalf("background launch LeadSessionID = %q, want auto-bg-session", res.LeadSessionID)
	}

	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(res.PID, &ws, 0, nil)
	st := StatusFor(res.JobID)
	if st.LeadSessionID != "auto-bg-session" {
		t.Fatalf("background status LeadSessionID = %q, want auto-bg-session", st.LeadSessionID)
	}
}

func TestStatusFor_DonePathClassifiesAndCaches(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	jobID := "job-done-1"
	// Synthesize a finished job: dead pid + captured success envelope.
	if err := writeMeta(dir, jobMeta{
		JobID: jobID, PID: deadReapedPID(t), Vendor: "glm", Model: "glm-4.6",
		StartedAt: time.Now().UTC().Format(time.RFC3339), Status: "running", JSON: true,
		LeadSessionID: "lead-done-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jobID+".out"), []byte(smokeSuccessJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	st := StatusFor(jobID)
	if !st.OK || st.Status != "done" || st.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("StatusFor done: %+v", st)
	}
	if st.Model != "mimo-v2-flash" {
		t.Fatalf("StatusFor should distill modelUsage key, got %q", st.Model)
	}
	if st.LeadSessionID != "lead-done-1" {
		t.Fatalf("StatusFor done should carry lead_session_id: %+v", st)
	}
	// Terminal result must be cached.
	if _, err := os.Stat(filepath.Join(dir, jobID+".result.json")); err != nil {
		t.Fatalf("result not cached: %v", err)
	}
	// A second call serves the cache and stays done.
	if st2 := StatusFor(jobID); st2.Status != "done" || !st2.OK {
		t.Fatalf("cached StatusFor: %+v", st2)
	}
}

func TestListJobs_MultipleSortedNewestFirst(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	mk := func(id, started string) {
		if err := writeMeta(dir, jobMeta{
			JobID: id, PID: deadReapedPID(t), Vendor: "glm", Model: "glm-4.6",
			StartedAt: started, Status: "running", JSON: true,
		}); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, id+".out"), []byte("{}"), 0o600)
	}
	mk("job-a", "2026-05-26T01:00:00Z")
	mk("job-c", "2026-05-26T03:00:00Z")
	mk("job-b", "2026-05-26T02:00:00Z")
	// A stray cached-result file with no matching meta must be skipped (not
	// treated as a job).
	_ = os.WriteFile(filepath.Join(dir, "orphan.result.json"), []byte("{}"), 0o600)

	jobs, err := ListJobs()
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("want 3 jobs, got %d: %+v", len(jobs), jobs)
	}
	wantOrder := []string{"job-c", "job-b", "job-a"} // newest StartedAt first
	for i, w := range wantOrder {
		if jobs[i].JobID != w {
			t.Fatalf("order[%d] = %q, want %q (newest first)", i, jobs[i].JobID, w)
		}
	}
}

func TestGC(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	// 1. finished + old → removable.
	finishedOld := "finished-old"
	_ = writeMeta(dir, jobMeta{JobID: finishedOld, PID: deadReapedPID(t), StartedAt: old, Status: "running"})
	_ = os.WriteFile(filepath.Join(dir, finishedOld+".out"), []byte("{}"), 0o600)

	// 2. finished but recent → kept by a 24h cutoff.
	finishedNew := "finished-new"
	_ = writeMeta(dir, jobMeta{JobID: finishedNew, PID: deadReapedPID(t), StartedAt: now, Status: "running"})

	// 3. alive (our pid) + old → kept (never GC a live job).
	aliveOld := "alive-old"
	_ = writeMeta(dir, jobMeta{JobID: aliveOld, PID: os.Getpid(), StartedAt: old, Status: "running"})

	gc := GC(24 * time.Hour)
	if !gc.OK || gc.Removed != 1 {
		t.Fatalf("GC removed = %d, want 1: %+v", gc.Removed, gc)
	}
	if _, err := os.Stat(filepath.Join(dir, finishedOld+".json")); !os.IsNotExist(err) {
		t.Fatalf("finished-old should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, finishedNew+".json")); err != nil {
		t.Fatalf("finished-new should be kept (too recent): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, aliveOld+".json")); err != nil {
		t.Fatalf("alive-old should be kept (process alive): %v", err)
	}
}
