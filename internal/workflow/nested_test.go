package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNestedWorkflowRunsAndReturnsResult: workflow(child) runs the child on the SAME
// engine (its leaf is tagged with the parent run), passes args, and returns the child's
// `result` global.
func TestNestedWorkflowRunsAndReturnsResult(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	dir := t.TempDir()
	child := filepath.Join(dir, "child.star")
	if err := os.WriteFile(child, []byte(`meta = {"name": "c", "description": "d"}
result = agent("child-task:" + args["topic"], vendor="v")
`), 0o600); err != nil {
		t.Fatal(err)
	}

	g, err := runScript(t, "nest1", 4, echoLeaf(rec),
		`got = workflow("`+child+`", args={"topic": "auth"})`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := asStr(t, g["got"]); s != "ok:child-task:auth" {
		t.Errorf("nested result = %q, want ok:child-task:auth", s)
	}
	if p := rec.prompts(); len(p) != 1 || p[0] != "child-task:auth" {
		t.Errorf("child leaf prompts = %v, want [child-task:auth]", p)
	}
}

// TestNestedWorkflowDepthGuard: a child that itself calls workflow() is rejected (one
// level deep only).
func TestNestedWorkflowDepthGuard(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	dir := t.TempDir()
	grandchild := filepath.Join(dir, "gc.star")
	os.WriteFile(grandchild, []byte(`meta = {"name": "gc", "description": "d"}
result = "deep"
`), 0o600)
	child := filepath.Join(dir, "child.star")
	os.WriteFile(child, []byte(`meta = {"name": "c", "description": "d"}
result = workflow("`+grandchild+`")
`), 0o600)

	_, err := runScript(t, "nest2", 4, echoLeaf(rec), `x = workflow("`+child+`")`)
	if err == nil || !strings.Contains(err.Error(), "one level deep") {
		t.Errorf("expected a depth-2 rejection, got %v", err)
	}
}
