package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNestedWorkflowRunsAndReturnsResult: workflow(child) runs the child on the SAME
// engine (its leaf is tagged with the parent run), passes args, and resolves to the
// child body's `return` value.
func TestNestedWorkflowRunsAndReturnsResult(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	child := filepath.Join(dir, "child.js")
	if err := os.WriteFile(child, []byte(`const meta = {name: "c", description: "d"};
return await agent("child-task:" + args.topic, {vendor: "v"});
`), 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := runScript(t, "nest1", 4, echoLeaf(rec),
		`return { got: await workflow("`+child+`", {topic: "auth"}) };`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := strField(t, wantMap(t, v), "got"); s != "ok:child-task:auth" {
		t.Errorf("nested result = %q, want ok:child-task:auth", s)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].prompt != "child-task:auth" {
		t.Fatalf("child leaf calls = %+v, want one child-task:auth leaf", calls)
	}
	if calls[0].runID != "nest1" {
		t.Errorf("child leaf runID = %q, want the parent run nest1", calls[0].runID)
	}
}

// TestNestedWorkflowDepthGuard: a child that itself calls workflow() is rejected (one
// level deep only).
func TestNestedWorkflowDepthGuard(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	grandchild := filepath.Join(dir, "gc.js")
	os.WriteFile(grandchild, []byte(`const meta = {name: "gc", description: "d"};
return "deep";
`), 0o600)
	child := filepath.Join(dir, "child.js")
	os.WriteFile(child, []byte(`const meta = {name: "c", description: "d"};
return await workflow("`+grandchild+`");
`), 0o600)

	_, err := runScript(t, "nest2", 4, echoLeaf(rec), `return await workflow("`+child+`");`)
	if err == nil || !strings.Contains(err.Error(), "one level deep") {
		t.Errorf("expected a depth-2 rejection, got %v", err)
	}
}

// TestNestedWorkflowGuardIsLexical: the one-level guard is the child body's `workflow`
// parameter itself, so a workflow() call from a child's parallel thunk throws it too —
// there is no dynamic path around the guard.
func TestNestedWorkflowGuardIsLexical(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()
	grandchild := filepath.Join(dir, "gc.js")
	os.WriteFile(grandchild, []byte(`const meta = {name: "gc", description: "d"};
return "deep";
`), 0o600)
	child := filepath.Join(dir, "child.js")
	os.WriteFile(child, []byte(`const meta = {name: "c", description: "d"};
let msg = "";
await parallel([async () => { try { await workflow("`+grandchild+`"); } catch (e) { msg = "" + e; } }]);
return msg;
`), 0o600)

	v, err := runScript(t, "nest3", 4, echoLeaf(rec),
		`return { msg: await workflow("`+child+`") };`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := strField(t, wantMap(t, v), "msg"); !strings.Contains(s, "one level deep") {
		t.Errorf("thunk workflow() error = %q, want the one-level guard", s)
	}
}
