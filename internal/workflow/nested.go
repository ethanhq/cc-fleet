package workflow

import (
	"fmt"
	"os"
	"path/filepath"

	"go.starlark.net/starlark"
)

// nestedLocalKey marks a thread that is executing inside a nested workflow() child (and,
// via fanout propagation, the parallel/pipeline goroutine threads it spawns). A workflow()
// call from such a thread is rejected — enforcing native's "one level deep only".
const nestedLocalKey = "wfNested"

// workflow runs another .star inline on the SAME engine — sharing its scheduler (one
// pool), journal (cache/resume), budget, live-event channel, AND its meta-derived
// settings — exactly one level deep. The child inherits the PARENT's meta.model (the
// default model for agents omitting model=) and whenToUse; the child's own `meta.model`/
// `whenToUse` are NOT binding (a single shared engine has one of each, and per-concurrent-
// child scoping would race a parent parallel that nests in several branches). A child that
// needs a specific model passes model= on its agent() calls. (The child's body still
// executes; its meta is just not re-read by the engine.)
// The child's leaves appear under a workflow group bracket on the board. Starlark module
// bodies have no top-level `return`, so the child returns a value by setting a module
// global named `result` (None if it doesn't). args= is passed to the child as its frozen
// `args` global.
//
// Re-entrancy is safe: workflow() is called with the GIL held, and ExecFileOptions runs
// the child body under that same held GIL while the parent thread is paused here — the
// child's own agent()/parallel() builtins do the usual unlock-around-exec/relock, so the
// single GIL is never double-locked.
func (e *engine) workflow(thread *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	var argsVal starlark.Value
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "script", &path, "args?", &argsVal); err != nil {
		return nil, err
	}
	if thread.Local(nestedLocalKey) != nil {
		return nil, fmt.Errorf("workflow: nested workflows are one level deep only")
	}
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		return nil, fmt.Errorf("workflow: resolve %q: %w", path, aerr)
	}
	src, rerr := os.ReadFile(abs)
	if rerr != nil {
		return nil, fmt.Errorf("workflow: read %q: %w", path, rerr)
	}

	child := e.builtins(Options{}) // SAME engine receiver → shares sched/journal/budget/events
	if argsVal != nil && argsVal != starlark.None {
		argsVal.Freeze()
		child["args"] = argsVal
	}

	gid := e.emitGroupOpen("workflow")
	defer e.emitGroupClose(gid)
	childThread := e.sched.newThread("workflow:" + e.runID + ":nested")
	childThread.SetLocal(nestedLocalKey, true)
	g, err := starlark.ExecFileOptions(fileOptions, childThread, abs, src, child)
	if err != nil {
		return nil, fmt.Errorf("workflow(%s): %w", filepath.Base(path), err)
	}
	if r, ok := g["result"]; ok {
		return r, nil
	}
	return starlark.None, nil
}
