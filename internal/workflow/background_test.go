package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestBackgroundLaunchAndAwait: an unawaited agent() promise is the background form —
// the leaf launches immediately and runs detached from script flow; awaiting the saved
// promise later yields its result, and a direct await still works alongside.
func TestBackgroundLaunchAndAwait(t *testing.T) {
	rec := &recorder{}
	v, err := runScript(t, "bg", 4, echoLeaf(rec), `
const p1 = agent("a", {vendor: "v"});
const p2 = agent("b", {vendor: "v"});
const thenable = typeof p1.then === "function";
const both = [await p1, await p2];
const one = await agent("c", {vendor: "v"});
return { thenable, both, one };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if m["thenable"] != true {
		t.Error("agent() must return a promise the script can hold and await later")
	}
	both := listField(t, m, "both")
	if len(both) != 2 || strAt(t, both, 0) != "ok:a" || strAt(t, both, 1) != "ok:b" {
		t.Errorf("awaited saved promises = %v, want [ok:a ok:b]", both)
	}
	if s := strField(t, m, "one"); s != "ok:c" {
		t.Errorf("one = %q, want ok:c", s)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("leaf execs = %d, want 3", n)
	}
}

// TestBackgroundSchemaResult: a schema leaf composes with the background form — the
// validated structured payload arrives when the saved promise is awaited.
func TestBackgroundSchemaResult(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		if c.prompt == "q" {
			return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a": 7}`)}
		}
		return subagent.Result{OK: true, Result: "ok:" + c.prompt}
	})
	v, err := runScript(t, "bgs", 2, leaf, `
const p = agent("q", {vendor: "v", schema: {required: ["a"]}});
await agent("other", {vendor: "v"});
const r = await p;
return { a: r.a };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "a"); n != 7 {
		t.Errorf("a = %v, want 7 (the structured payload)", n)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("leaf execs = %d, want 2", n)
	}
}

// TestDoubleAwaitNoDoubleCharge: awaiting the same leaf promise twice returns the same
// settled value with ONE exec and ONE budget charge — a promise settles once.
func TestDoubleAwaitNoDoubleCharge(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "bgw", 2, costLeaf(rec, 0.5))
	eng.budgetTotal = 100
	v, err := eng.run("bgw.js", []byte(`
const p = agent("a", {vendor: "v"});
const r1 = await p;
const r2 = await p;
return { same: r1 === r2, r1, sp: budget.spent() };
`), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if m["same"] != true {
		t.Error("both awaits must observe the same settled value")
	}
	if s := strField(t, m, "r1"); s != "ok:a" {
		t.Errorf("r1 = %q, want ok:a", s)
	}
	if f := budgetFloat(t, m, "sp"); f != 0.5 {
		t.Errorf("budget spent = %v, want 0.5 (cost counted once, not per await)", f)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("leaf execs = %d, want 1 (second await served from the settled promise)", n)
	}
}

// TestAwaitBackgroundHonorsCancel: a script awaiting a never-finishing background leaf
// returns promptly as a stop when the run ctx is cancelled — the leaf's exec ctx dies
// and the run finalizes "stopped", not a hang.
func TestAwaitBackgroundHonorsCancel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	started := make(chan struct{}, 1)
	leaf := func(ctx context.Context, req subagent.Request) subagent.Result {
		started <- struct{}{}
		<-ctx.Done()
		return subagent.Result{OK: false, ErrorCode: "SUBAGENT_STOPPED", ErrorMsg: "stopped"}
	}
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })

	ctx, cancel := context.WithCancel(context.Background())
	eng := newTestEngine(ctx, "bgc", 2)
	done := make(chan error, 1)
	go func() {
		_, err := eng.run("bgc.js", []byte(`const p = agent("a", {vendor: "v"}); return await p;`), Options{})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "run stopped") {
			t.Errorf("err = %v, want a run-stopped error", err)
		}
		if !eng.stopped {
			t.Error("eng.stopped not set — the manifest would finalize failed, not stopped")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel — possible hang")
	}
}

// TestBackgroundOmittedTimeoutDelegates: a background leaf without timeout reaches the
// exec with Timeout 0 — the subagent sync default bounds it there; the engine adds no
// backstop of its own.
func TestBackgroundOmittedTimeoutDelegates(t *testing.T) {
	rec := &recorder{}
	_, err := runScript(t, "bgd", 2, echoLeaf(rec), `
const p = agent("a", {vendor: "v"});
await p;
return {};
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c := rec.snapshot()[0]; c.timeout != 0 {
		t.Errorf("omitted timeout = %v, want 0 (the subagent sync default applies at exec)", c.timeout)
	}
}

// TestBackgroundJournalsAtCompletion: a background leaf journals when it completes even
// if the script never awaits it — a later invocation under the same run id serves BOTH
// leaves from the journal with zero re-exec.
func TestBackgroundJournalsAtCompletion(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })

	const runID = "bgj"
	src := `agent("a", {vendor: "v"});
return await agent("b", {vendor: "v"});`
	if _, err := newEngineFor(t, runID, 2).run("bgj.js", []byte(src), Options{}); err != nil {
		t.Fatalf("pass1: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Fatalf("pass1 execs = %d, want 2", n)
	}
	if _, err := newEngineFor(t, runID, 2).run("bgj.js", []byte(src), Options{}); err != nil {
		t.Fatalf("pass2: %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("pass2 execs = %d, want 2 (the never-awaited leaf journaled at completion)", n)
	}
}
