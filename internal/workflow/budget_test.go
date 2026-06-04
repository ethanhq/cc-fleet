package workflow

import (
	"context"
	"math"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

func costLeaf(rec *recorder, usd float64) func(subagent.Request) subagent.Result {
	return fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt, CostUSD: usd}
	})
}

// TestBudgetCapsRun: with a $1.00 cap and $0.50/leaf, the 3rd agent() (spent already
// $1.00 >= cap) raises a budget error and aborts the run — exactly 2 leaves executed.
func TestBudgetCapsRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = costLeaf(rec, 0.5)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "bud", budgetTotal: 1.0}
	_, err := eng.run("b.star", `
for i in range(10):
    agent("x%d" % i, vendor="v")
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected a budget-exceeded error, got %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("ran %d leaves, want 2 (cap $1.00 at $0.50/leaf)", n)
	}
}

// TestBudgetSpentRemainingTotal: spent()/remaining()/total reflect accumulated CostUSD.
func TestBudgetSpentRemainingTotal(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = costLeaf(rec, 0.25)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "bud2", budgetTotal: 2.0}
	g, err := eng.run("b.star", `
agent("a", vendor="v")
agent("b", vendor="v")
sp = budget.spent()
rem = budget.remaining()
tot = budget.total
`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if f, _ := starlark.AsFloat(g["sp"]); f != 0.5 {
		t.Errorf("spent = %v, want 0.5", g["sp"])
	}
	if f, _ := starlark.AsFloat(g["rem"]); f != 1.5 {
		t.Errorf("remaining = %v, want 1.5", g["rem"])
	}
	if f, _ := starlark.AsFloat(g["tot"]); f != 2.0 {
		t.Errorf("total = %v, want 2.0", g["tot"])
	}
}

// TestBudgetUncapped: with no cap, total is None and remaining() is +Inf — so a
// `while budget.remaining() > X` loop is unbounded by budget (only the lifetime cap).
func TestBudgetUncapped(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = costLeaf(rec, 0.1)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "bud3"} // budgetTotal 0 = uncapped
	g, err := eng.run("b.star", `
agent("a", vendor="v")
tot = budget.total
rem = budget.remaining()
`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if g["tot"] != starlark.None {
		t.Errorf("uncapped total = %v, want None", g["tot"])
	}
	if f, _ := starlark.AsFloat(g["rem"]); !math.IsInf(f, 1) {
		t.Errorf("uncapped remaining = %v, want +Inf", g["rem"])
	}
}
