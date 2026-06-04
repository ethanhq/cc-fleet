// Package workflow is cc-fleet's deterministic orchestration runtime: it runs a
// Starlark script that fans out vendor subagent leaves, in a cc-fleet process OFF the
// main Claude context. The script's plan lives in Starlark variables (CPU, ~0 tokens);
// the model is invoked only at agent() leaves. It mirrors the native Claude Code
// Workflow API (meta / agent / parallel / pipeline / phase / log); the only shape
// differences are agent()'s vendor= parameter and Starlark syntax.
//
// Concurrency is a GIL (see sched.go): one mutex serializes ALL Starlark interpreter
// execution, released only around the blocking vendor exec, so the engine is -race
// clean while the slow leaves still overlap up to a bounded pool.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// fileOptions are the resolver/compiler options for every workflow script:
// top-level for/if allowed (the body reads like the native JS body); `while` disabled
// (forces a bounded `for _ in range(N): … break`, complementing the lifetime cap);
// recursion disabled (default); `set` allowed; no global reassignment (globals freeze
// after load → safe concurrent reads from parallel/pipeline goroutines).
var fileOptions = &syntax.FileOptions{
	TopLevelControl: true,
	While:           false,
	GlobalReassign:  false,
	Recursion:       false,
	Set:             true,
}

// runLeaf is the vendor-subagent leaf — a seam so tests inject a deterministic fake
// in place of a real `claude -p` exec. Production = subagent.Run (in-process,
// key-safe via apiKeyHelper, board-registered, tagged with run/phase/label).
var runLeaf = subagent.Run

// Options configures a workflow run.
type Options struct {
	// RunID, when set, names an EXISTING manifest to execute (the detached child /
	// foreground re-exec path); empty means Prepare mints a fresh one.
	RunID       string
	Concurrency int    // 0 → defaultConcurrency()
	ArgsJSON    string // optional; predeclared to the script as `args`
}

// Prepare parses a script, extracts + validates its `meta` literal, and mints a run
// manifest with the name/description/declared phases — BEFORE any execution, so a bad
// script never mints a half-run and the board shows the named, phase-skeletoned run
// immediately. Returns the new manifest (its RunID is handed to a detached child or
// printed to the caller).
func Prepare(scriptPath string) (subagent.WorkflowRun, error) {
	src, err := os.ReadFile(scriptPath)
	if err != nil {
		return subagent.WorkflowRun{}, fmt.Errorf("workflow: read script: %w", err)
	}
	meta, err := extractMeta(fileOptions, scriptPath, src)
	if err != nil {
		return subagent.WorkflowRun{}, err
	}
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}
	return subagent.NewRunWithMeta(meta.Name, meta.Description, phases)
}

// Execute runs a prepared script's body to completion in the CURRENT process,
// tagging every leaf with runID, and flips the manifest to done/failed on exit. It
// NEVER lets a panic escape: a panic (anywhere on the top-level thread) is recovered
// into a failed status so a detached run always finalizes. (Goroutine panics inside
// parallel/pipeline are recovered at their own boundary in callOrNone.)
func Execute(ctx context.Context, scriptPath, runID string, opts Options) (err error) {
	// Fail-fast on a bad run id (e.g. a malformed `--run-id`) before doing anything —
	// it becomes a manifest path component, and a doomed-to-not-persist run shouldn't run.
	if verr := subagent.ValidateRunID(runID); verr != nil {
		return fmt.Errorf("workflow: invalid run id: %w", verr)
	}
	// Seed the engine's authoritative manifest state from the Prepare-minted manifest
	// (best-effort) + the script's meta. The engine then OWNS the manifest, overwriting
	// it whole on every phase()/finalize — so there is no read-modify-write to race and
	// a concurrently-dropped manifest is recreated on the next write.
	prepared, _ := subagent.ReadRun(runID) // the minted manifest, if still present
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if prepared.StartedAt != "" {
		startedAt = prepared.StartedAt
	}
	// On a pre-execution failure, finalize the run as failed WITHOUT dropping the
	// prepared name/description/phases (only status + the failure cause change).
	failManifest := func(cause error) {
		prepared.RunID = runID
		if prepared.StartedAt == "" {
			prepared.StartedAt = startedAt
		}
		prepared.Status = "failed"
		prepared.Error = cause.Error()
		_ = subagent.SaveRun(prepared)
	}

	src, rerr := os.ReadFile(scriptPath)
	if rerr != nil {
		e := fmt.Errorf("workflow: read script: %w", rerr)
		failManifest(e)
		return e
	}
	meta, merr := extractMeta(fileOptions, scriptPath, src)
	if merr != nil {
		failManifest(merr)
		return merr
	}
	phases := make([]subagent.RunPhase, 0, len(meta.Phases))
	for _, p := range meta.Phases {
		phases = append(phases, subagent.RunPhase{Title: p.Title, Detail: p.Detail})
	}

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency()
	}
	eng := &engine{
		sched: newScheduler(ctx, concurrency), runID: runID,
		name: meta.Name, description: meta.Description, startedAt: startedAt, phases: phases,
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("workflow: script panicked: %v", r)
		}
		status, errText := "done", ""
		if err != nil {
			status, errText = "failed", err.Error()
		}
		eng.saveManifest(status, errText)
	}()

	_, execErr := eng.run(scriptPath, src, opts)
	return execErr
}

// run executes a script body under the GIL and returns its module globals. The top
// level holds the GIL; every builtin returns with the GIL held (runBlocking's
// defer-lock invariant), so this unlock is balanced on the normal path. On a panic
// the unlock is skipped, leaving the GIL locked — harmless, the run is ending and the
// caller's recover finalizes. Shared by Execute and the tests (which assert on the
// returned globals).
func (eng *engine) run(scriptPath string, src interface{}, opts Options) (starlark.StringDict, error) {
	predeclared := eng.builtins(opts)
	thread := eng.sched.newThread("workflow:" + eng.runID)
	eng.sched.lock()
	g, err := starlark.ExecFileOptions(fileOptions, thread, scriptPath, src, predeclared)
	eng.sched.unlock()
	return g, err
}

// Launch is the entry for `cc-fleet workflow run`. It prepares the run (parse + meta +
// mint), then either runs it inline (foreground — the debug / deterministic-e2e path)
// or re-execs cc-fleet as a DETACHED child that runs it to completion off the launching
// process, returning the run id immediately. Detaching reuses the subagent leaf's
// proven process-group primitive — no new platform split.
func Launch(ctx context.Context, scriptPath string, opts Options, foreground bool) (string, error) {
	abs, err := filepath.Abs(scriptPath)
	if err != nil {
		return "", fmt.Errorf("workflow: resolve script path: %w", err)
	}
	if opts.ArgsJSON != "" && !json.Valid([]byte(opts.ArgsJSON)) {
		return "", fmt.Errorf("workflow: --args-json is not valid JSON")
	}
	run, err := Prepare(abs)
	if err != nil {
		return "", err
	}
	if foreground {
		return run.RunID, Execute(ctx, abs, run.RunID, opts)
	}
	if lerr := launchDetached(abs, run.RunID, opts); lerr != nil {
		run.Status = "failed"
		_ = subagent.SaveRun(run)
		return "", lerr
	}
	return run.RunID, nil
}
