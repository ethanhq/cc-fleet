package subagent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSaveWorkflow_RoundTrip: SaveWorkflow copies a run's .star to a named store; reuse resolves it,
// list reports the metadata, and a path-traversal name / absent name are rejected.
func TestSaveWorkflow_RoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := NewRun("r", nil)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	sp, _ := RunScriptPath(run.RunID)
	_ = os.MkdirAll(filepath.Dir(sp), 0o700)
	if err := os.WriteFile(sp, []byte("meta={}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkflow(run.RunID, "my-flow", "sess-1", "desc"); err != nil {
		t.Fatalf("SaveWorkflow: %v", err)
	}
	star, err := SavedWorkflowScript("my-flow")
	if err != nil {
		t.Fatalf("SavedWorkflowScript: %v", err)
	}
	if b, _ := os.ReadFile(star); string(b) != "meta={}" {
		t.Fatalf("saved script content mismatch: %q", b)
	}
	list, _ := ListSavedWorkflows()
	if len(list) != 1 || list[0].Name != "my-flow" || list[0].SessionID != "sess-1" {
		t.Fatalf("ListSavedWorkflows mismatch: %+v", list)
	}
	if err := SaveWorkflow(run.RunID, "../escape", "", ""); err == nil {
		t.Fatal("a path-traversal name must be rejected")
	}
	if _, err := SavedWorkflowScript("nope"); err == nil {
		t.Fatal("an absent saved workflow must error")
	}
}
