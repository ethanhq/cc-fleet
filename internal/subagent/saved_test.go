package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveWorkflow_RoundTrip: SaveWorkflow copies a run's .js to a named store; reuse resolves it,
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
	if err := os.WriteFile(sp, []byte("const meta = {};"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkflow(run.RunID, "my-flow", "sess-1", "desc"); err != nil {
		t.Fatalf("SaveWorkflow: %v", err)
	}
	script, err := SavedWorkflowScript("my-flow")
	if err != nil {
		t.Fatalf("SavedWorkflowScript: %v", err)
	}
	if b, _ := os.ReadFile(script); string(b) != "const meta = {};" {
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

// TestSaveWorkflow_LegacyStarRefused: a run carrying only a Starlark .star script can't be saved,
// and a workflow saved by the retired Starlark engine can't be resolved for running.
func TestSaveWorkflow_LegacyStarRefused(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	run, err := NewRun("r", nil)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	lp, _ := LegacyRunScriptPath(run.RunID)
	_ = os.MkdirAll(filepath.Dir(lp), 0o700)
	if err := os.WriteFile(lp, []byte("meta = {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = SaveWorkflow(run.RunID, "old-flow", "", "")
	if err == nil || !strings.Contains(err.Error(), "predates the JavaScript workflow engine") {
		t.Fatalf("SaveWorkflow on a .star-only run = %v, want a predates-the-JS-engine error", err)
	}

	legacy, err := savedPath("starlark-save", ".star")
	if err != nil {
		t.Fatalf("savedPath: %v", err)
	}
	_ = os.MkdirAll(filepath.Dir(legacy), 0o700)
	if err := os.WriteFile(legacy, []byte("meta = {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SavedWorkflowScript("starlark-save"); err == nil || !strings.Contains(err.Error(), "was saved by the retired Starlark engine") {
		t.Fatalf("SavedWorkflowScript on a .star-only save = %v, want a retired-Starlark-engine error", err)
	}
}
