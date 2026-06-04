package subagent

import (
	"testing"
)

// Cross-platform run-grouping test. The fake-claude exec cases live in
// run_grouping_unix_test.go.

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
