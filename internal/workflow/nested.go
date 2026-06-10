package workflow

import (
	"os"
	"path/filepath"

	"github.com/dop251/goja"
)

// jsWorkflow runs another .js script inline on the SAME engine — sharing its scheduler
// (one pool), journal (cache/resume), budget, live-event channel, AND its meta-derived
// settings — exactly one level deep: the parent body's `workflow` parameter is this
// function, while the child body's is a stub that only throws (the guard is lexical, so
// it also covers workflow() calls from the child's parallel/pipeline thunks). The guard
// targets ACCIDENTAL depth, not a deliberately adversarial author: a script polluting
// Object.prototype/globalThis to hand its child the real function only recurses its own
// engine process — leaf spend stays bound by the lifetime/budget caps and the process by
// `workflow stop` — the same self-inflicted, externally-stoppable class as a busy spin.
// The child inherits the PARENT's meta.model and whenToUse; its own `const meta` stays scoped to
// its wrapper and is not re-read by the engine (a single shared engine has one of each).
// args is passed as the child's own `args` parameter. Returns the child's async-IIFE
// promise, with the group bracket closed — and a rejection relabeled with the script
// name — when it settles. Runs on the loop.
func (e *engine) jsWorkflow(call goja.FunctionCall) goja.Value {
	path, ok := call.Argument(0).Export().(string)
	if !ok || path == "" {
		panic(e.newError("workflow: script must be a path string"))
	}
	// args crosses the child boundary as a JSON clone (the VM's own stringify→parse):
	// plain data passes through; a function — e.g. the parent smuggling its own
	// `workflow` capability past the one-level guard — cannot.
	childArgs := goja.Value(goja.Undefined())
	if a := call.Argument(1); !goja.IsUndefined(a) && !goja.IsNull(a) {
		cloned, cerr := e.jsonClone(a)
		if cerr != nil {
			panic(e.newError("workflow: args must be a JSON-serializable value: %v", cerr))
		}
		childArgs = cloned
	}
	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		panic(e.newError("workflow: resolve %q: %v", path, aerr))
	}
	base := filepath.Base(path)
	src, rerr := os.ReadFile(abs)
	if rerr != nil {
		panic(e.newError("workflow: read %q: %v", path, rerr))
	}
	normalized, _, nerr := normalizeScript(abs, src)
	if nerr != nil {
		panic(e.newError("workflow(%s): %v", base, nerr))
	}
	prog, cerr := goja.Compile(abs, wrapScript(normalized), false)
	if cerr != nil {
		panic(e.newError("workflow(%s): %v", base, cerr))
	}
	fnVal, xerr := e.vm.RunProgram(prog)
	if xerr != nil {
		panic(e.newError("workflow(%s): %v", base, xerr))
	}
	childFn, ok := goja.AssertFunction(fnVal)
	if !ok {
		panic(e.newError("workflow(%s): script did not compile to a callable body", base))
	}
	gid := e.emitGroupOpen("workflow")
	pv, perr := childFn(goja.Undefined(), e.nestedStub, childArgs)
	if perr != nil {
		e.emitGroupClose(gid)
		panic(e.newError("workflow(%s): %v", base, perr))
	}
	pobj := pv.ToObject(e.vm)
	then, ok := goja.AssertFunction(pobj.Get("then"))
	if !ok {
		e.emitGroupClose(gid)
		panic(e.newError("workflow(%s): script body did not produce a promise", base))
	}
	chained, terr := then(pobj,
		e.vm.ToValue(func(c goja.FunctionCall) goja.Value {
			e.emitGroupClose(gid)
			return c.Argument(0)
		}),
		e.vm.ToValue(func(c goja.FunctionCall) goja.Value {
			e.emitGroupClose(gid)
			panic(e.newError("workflow(%s): %v", base, rejectionError(c.Argument(0))))
		}))
	if terr != nil {
		e.emitGroupClose(gid)
		panic(e.newError("workflow(%s): %v", base, terr))
	}
	return chained
}
