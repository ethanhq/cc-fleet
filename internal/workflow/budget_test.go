package workflow

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestNormalizeBudgetSentinels: the --no-budget -1 sentinel becomes 0 (explicit uncap); 0 and positive
// values pass through — so the engine and manifest never see -1, and a token cap survives a USD uncap.
func TestNormalizeBudgetSentinels(t *testing.T) {
	cases := []struct {
		inUSD, wantUSD float64
		inTok, wantTok int64
	}{
		{-1, 0, -1, 0},
		{0, 0, 0, 0},
		{20, 20, 500_000, 500_000},
		{-1, 0, 500_000, 500_000}, // uncap USD, keep the token cap
	}
	for _, c := range cases {
		opts := Options{BudgetUSD: c.inUSD, BudgetTokens: c.inTok}
		normalizeBudgetSentinels(&opts)
		if opts.BudgetUSD != c.wantUSD || opts.BudgetTokens != c.wantTok {
			t.Errorf("normalize($%v,%d) = ($%v,%d), want ($%v,%d)", c.inUSD, c.inTok, opts.BudgetUSD, opts.BudgetTokens, c.wantUSD, c.wantTok)
		}
	}
}

// TestResumeBudgetInheritAndUncap exercises uncap-on-resume through the real Launch→manifest path: a
// plain resume inherits both caps off the manifest; --no-budget (the -1 sentinel) durably uncaps BOTH
// to 0 (the engine never persists -1). Foreground so it runs inline without a detached child.
func TestResumeBudgetInheritAndUncap(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	ctx := context.Background()

	dir := t.TempDir()
	script := filepath.Join(dir, "w.star")
	if err := os.WriteFile(script, []byte("meta = {\"name\": \"n\", \"description\": \"d\"}\nx = agent(\"a\", vendor=\"v\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := Launch(ctx, script, Options{BudgetUSD: 20, BudgetTokens: 500_000}, true)
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	if r, _ := subagent.ReadRun(id); r.BudgetUSD != 20 || r.BudgetTokens != 500_000 {
		t.Fatalf("fresh manifest budgets = $%v / %d tok, want 20 / 500000", r.BudgetUSD, r.BudgetTokens)
	}

	if _, err := Launch(ctx, script, Options{Resume: id}, true); err != nil {
		t.Fatalf("plain resume: %v", err)
	}
	if r, _ := subagent.ReadRun(id); r.BudgetUSD != 20 || r.BudgetTokens != 500_000 {
		t.Errorf("plain resume should inherit both caps, got $%v / %d", r.BudgetUSD, r.BudgetTokens)
	}

	if _, err := Launch(ctx, script, Options{Resume: id, BudgetUSD: -1, BudgetTokens: -1}, true); err != nil {
		t.Fatalf("uncap resume: %v", err)
	}
	if r, _ := subagent.ReadRun(id); r.BudgetUSD != 0 || r.BudgetTokens != 0 {
		t.Errorf("--no-budget resume should durably uncap to 0/0, got $%v / %d", r.BudgetUSD, r.BudgetTokens)
	}
}

func costLeaf(rec *recorder, usd float64) func(subagent.Request) subagent.Result {
	return fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt, CostUSD: usd}
	})
}

func tokenLeaf(rec *recorder, in, out int) func(subagent.Request) subagent.Result {
	return fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt, Usage: &subagent.Usage{InputTokens: in, OutputTokens: out}}
	})
}

// TestTokenBudgetCapsRun: the token cap aborts the run like the USD cap, via the same reservation
// (a flat 50_000-token estimate per leaf). With a 100_000-token cap and 50_000 real tokens/leaf
// (input 40k + output 10k = the reservation, so the gate is exact) exactly 2 leaves run, then the 3rd
// aborts. The USD cap is unset (CostUSD 0), so only the token cap gates.
func TestTokenBudgetCapsRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = tokenLeaf(rec, 40_000, 10_000)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "tbud", budgetTokensTotal: 100_000}
	_, err := eng.run("b.star", `
for i in range(10):
    agent("x%d" % i, vendor="v")
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected a token-budget-exceeded error, got %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("ran %d leaves, want 2 (cap 100k tokens at 50k/leaf)", n)
	}
}

// TestBudgetTokenObject: the budget object exposes the token cap as Int — tokens_total / tokens_spent()
// / tokens_remaining() reflect summed input+output (cache-read excluded), uncapped reads None/MaxInt64.
func TestBudgetTokenObject(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = tokenLeaf(rec, 40_000, 10_000) // 50k tokens/leaf
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "tbud2", budgetTokensTotal: 1_000_000}
	g, err := eng.run("b.star", `
agent("a", vendor="v")
agent("b", vendor="v")
ts = budget.tokens_spent()
tr = budget.tokens_remaining()
tt = budget.tokens_total
uncapped_total = budget.total
`, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["ts"]); i != 100_000 {
		t.Errorf("tokens_spent = %v, want 100000", g["ts"])
	}
	if i, _ := starlark.AsInt32(g["tr"]); i != 900_000 {
		t.Errorf("tokens_remaining = %v, want 900000", g["tr"])
	}
	if i, _ := starlark.AsInt32(g["tt"]); i != 1_000_000 {
		t.Errorf("tokens_total = %v, want 1000000", g["tt"])
	}
	if g["uncapped_total"] != starlark.None {
		t.Errorf("USD total (uncapped) = %v, want None", g["uncapped_total"])
	}
}

// TestBudgetCapsRun: the USD cap aborts the run via the pessimistic reservation. Each leaf reserves
// max(its max_budget_usd, the $1.00 default) = $1.00 against the cap until it reconciles to real; with
// a $2.00 cap and $1.00/leaf (reservation == real, so the gate is exact) exactly 2 leaves run, then the
// 3rd (spent $2.00 + its $1.00 reservation > $2.00) aborts.
func TestBudgetCapsRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = costLeaf(rec, 1.0)
	t.Cleanup(func() { runLeaf = old })

	eng := &engine{sched: newScheduler(context.Background(), 1), runID: "bud", budgetTotal: 2.0}
	_, err := eng.run("b.star", `
for i in range(10):
    agent("x%d" % i, vendor="v")
`, Options{})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected a budget-exceeded error, got %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("ran %d leaves, want 2 (cap $2.00 at $1.00/leaf)", n)
	}
}

// TestBudgetReservationBoundsConcurrentOvershoot: a parallel() fan-out under a cap used to admit EVERY
// leaf while spent was still ~0 (the charge only lands at completion), so a $20 cap could overshoot to
// $90. With the pessimistic reservation, a concurrent batch admits against spent+reserved, so the real
// total spend never exceeds the cap by more than ONE leaf's estimate.
func TestBudgetReservationBoundsConcurrentOvershoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	// Each leaf costs $1.00 — exactly its reservation, so spent tracks reserved with no slack.
	runLeaf = costLeaf(rec, 1.0)
	t.Cleanup(func() { runLeaf = old })

	// A $5.00 cap with a wide pool: parallel(20) leaves all race the gate. Without reservation every
	// one would admit (spent ~0) and charge → $20 spent. With it, admission stops near the cap.
	eng := &engine{sched: newScheduler(context.Background(), 8), runID: "bres", budgetTotal: 5.0}
	_, _ = eng.run("b.star", `
parallel([lambda i=i: agent("x%d" % i, vendor="v") for i in range(20)])
`, Options{})
	if spent := eng.budgetSpent; spent > 5.0+1.0 { // cap + one leaf's estimate (the accepted residual)
		t.Errorf("real spend $%.2f overshot the $5.00 cap by more than one leaf's $1.00 estimate", spent)
	}
	if eng.budgetReserved != 0 {
		t.Errorf("every reservation must be released; budgetReserved = %v, want 0", eng.budgetReserved)
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
