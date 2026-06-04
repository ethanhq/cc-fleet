package subagent

import (
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// TestSyncJobCarriesRunGrouping: a sync run tagged with run/phase/label threads
// those through registerSyncJob → the board → finalizeSyncJob's cached result,
// so a workflow can group its sync subagents.
func TestSyncJobCarriesRunGrouping(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	jobID := registerSyncJob(Request{Vendor: "glm", RunID: "run-1", Phase: "build", Label: "w1"}, "glm-4.6")
	if jobID == "" {
		t.Fatal("registerSyncJob returned an empty job id")
	}
	jobs, err := ListJobs()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("ListJobs: err=%v n=%d", err, len(jobs))
	}
	if jobs[0].RunID != "run-1" || jobs[0].Phase != "build" || jobs[0].Label != "w1" {
		t.Fatalf("running sync job missing run grouping: %+v", jobs[0])
	}

	finalizeSyncJob(jobID, Result{OK: true, Vendor: "glm", Model: "glm-4.6", Result: "answer"})
	jobs, _ = ListJobs()
	if len(jobs) != 1 || jobs[0].Status != "done" {
		t.Fatalf("after finalize want 1 done job: %+v", jobs)
	}
	if jobs[0].RunID != "run-1" || jobs[0].Phase != "build" || jobs[0].Label != "w1" {
		t.Fatalf("finalized sync job lost run grouping: %+v", jobs[0])
	}
}

// TestBackgroundJobCarriesRunGrouping: a --background launch tags its on-disk
// meta + the returned handle, and StatusFor surfaces the tags on a later poll.
func TestBackgroundJobCarriesRunGrouping(t *testing.T) {
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

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true, Background: true,
		RunID: "run-bg", Phase: "verify", Label: "v2"})
	if !res.OK || res.JobID == "" {
		t.Fatalf("background launch failed: %+v", res)
	}
	if res.RunID != "run-bg" || res.Phase != "verify" || res.Label != "v2" {
		t.Fatalf("background handle missing run grouping: %+v", res)
	}
	if st := StatusFor(res.JobID); st.RunID != "run-bg" || st.Phase != "verify" || st.Label != "v2" {
		t.Fatalf("StatusFor missing run grouping: %+v", st)
	}
}

// TestRun_SyncResultCarriesRunGrouping: a sync Run echoes the run grouping back
// on its returned Result (the caller's own envelope).
func TestRun_SyncResultCarriesRunGrouping(t *testing.T) {
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

	res := Run(Request{Vendor: "glm", Prompt: "hi", JSON: true,
		RunID: "run-sync", Phase: "review", Label: "r3"})
	if res.RunID != "run-sync" || res.Phase != "review" || res.Label != "r3" {
		t.Fatalf("sync Run result missing run grouping: %+v", res)
	}
}
