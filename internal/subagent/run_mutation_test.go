package subagent

import "testing"

// TestRunManifestAPI pins the manifest API the workflow runtime relies on:
// NewRunWithMeta round-trips Description + mints status "running", and SaveRun
// overwrites the whole manifest (the engine's single-writer model) and recreates a
// manifest that no longer exists on disk.
func TestRunManifestAPI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	run, err := NewRunWithMeta("nm", "the description", []RunPhase{{Title: "a"}})
	if err != nil {
		t.Fatalf("NewRunWithMeta: %v", err)
	}
	if run.Description != "the description" || run.Status != "running" {
		t.Errorf("minted run = %+v, want description round-tripped + status running", run)
	}

	// SaveRun overwrites the full manifest from caller-held state.
	run.Status = "done"
	run.Phases = append(run.Phases, RunPhase{Title: "b"})
	if err := SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	got, err := ReadRun(run.RunID)
	if err != nil {
		t.Fatalf("ReadRun: %v", err)
	}
	if got.Status != "done" || got.Description != "the description" ||
		len(got.Phases) != 2 || got.Phases[0].Title != "a" || got.Phases[1].Title != "b" {
		t.Errorf("after SaveRun: %+v, want status done, desc kept, phases [a b]", got)
	}

	// SaveRun recreates a manifest that was removed from disk (the GC-self-heal path).
	dir, _ := runsDir()
	removeRun(dir, run.RunID)
	if _, err := ReadRun(run.RunID); err == nil {
		t.Fatal("manifest should be gone after removeRun")
	}
	if err := SaveRun(run); err != nil {
		t.Fatalf("SaveRun recreate: %v", err)
	}
	if _, err := ReadRun(run.RunID); err != nil {
		t.Errorf("SaveRun should recreate a removed manifest: %v", err)
	}

	// SaveRun must reject a path-unsafe run id (it becomes a manifest path component).
	if err := SaveRun(WorkflowRun{RunID: "../escape", Status: "running"}); err == nil {
		t.Error("SaveRun must reject a path-traversal run id")
	}
}
