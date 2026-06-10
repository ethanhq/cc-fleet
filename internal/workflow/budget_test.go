package workflow

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	ctx := context.Background()

	dir := t.TempDir()
	script := filepath.Join(dir, "w.js")
	src := "const meta = {name: \"n\", description: \"d\"};\nawait agent(\"a\", {provider: \"v\"});\n"
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
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

func costLeaf(rec *recorder, usd float64) func(context.Context, subagent.Request) subagent.Result {
	return fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt, CostUSD: usd}
	})
}

func tokenLeaf(rec *recorder, in, out int) func(context.Context, subagent.Request) subagent.Result {
	return fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: "ok:" + c.prompt, Usage: &subagent.Usage{InputTokens: in, OutputTokens: out}}
	})
}

// budgetEngine wires a fake-leaf engine the runScript way (temp ConfigDir, leaf stub,
// resolveProfile pinned to identity) but returns the engine itself, so a test can set a
// budget cap before running.
func budgetEngine(t *testing.T, runID string, concurrency int, leaf func(context.Context, subagent.Request) subagent.Result) *engine {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = leaf
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	return newTestEngine(context.Background(), runID, concurrency)
}

// budgetFloat reads a numeric field as float64 (integral numbers export as int64).
func budgetFloat(t *testing.T, m map[string]interface{}, name string) float64 {
	t.Helper()
	switch n := m[name].(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	}
	t.Fatalf("field %q is %T (%v), want number", name, m[name], m[name])
	return 0
}

// TestTokenBudgetCapsRun: the token cap aborts the run like the USD cap, via the same reservation
// (a flat 50_000-token estimate per leaf). With a 100_000-token cap and 50_000 real tokens/leaf
// (input 40k + output 10k = the reservation, so the gate is exact) exactly 2 leaves run, then the 3rd
// throws. The USD cap is unset (CostUSD 0), so only the token cap gates.
func TestTokenBudgetCapsRun(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "tbud", 1, tokenLeaf(rec, 40_000, 10_000))
	eng.budgetTokensTotal = 100_000
	_, err := eng.run("b.js", []byte(`
for (let i = 0; i < 10; i++) {
    await agent("x" + i, {provider: "v"});
}
return {};
`), Options{})
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected a token-budget-exceeded error, got %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("ran %d leaves, want 2 (cap 100k tokens at 50k/leaf)", n)
	}
}

// TestBudgetTokenObject: the budget object exposes the token cap as integers — tokens_total /
// tokens_spent() / tokens_remaining() reflect summed input+output (cache-read excluded), and the
// uncapped USD total reads null.
func TestBudgetTokenObject(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "tbud2", 1, tokenLeaf(rec, 40_000, 10_000)) // 50k tokens/leaf
	eng.budgetTokensTotal = 1_000_000
	v, err := eng.run("b.js", []byte(`
await agent("a", {provider: "v"});
await agent("b", {provider: "v"});
return {
    ts: budget.tokens_spent(),
    tr: budget.tokens_remaining(),
    tt: budget.tokens_total,
    usdTotal: budget.total,
};
`), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if n := intField(t, m, "ts"); n != 100_000 {
		t.Errorf("tokens_spent = %v, want 100000", n)
	}
	if n := intField(t, m, "tr"); n != 900_000 {
		t.Errorf("tokens_remaining = %v, want 900000", n)
	}
	if n := intField(t, m, "tt"); n != 1_000_000 {
		t.Errorf("tokens_total = %v, want 1000000", n)
	}
	if uv, ok := m["usdTotal"]; !ok || uv != nil {
		t.Errorf("USD total (uncapped) = %v, want null", uv)
	}
}

// TestBudgetCapsRun: the USD cap aborts the run via the pessimistic reservation. Each leaf reserves
// max(its max_budget_usd, the $1.00 default) = $1.00 against the cap until it reconciles to real; with
// a $2.00 cap and $1.00/leaf (reservation == real, so the gate is exact) exactly 2 leaves run, then the
// 3rd (spent $2.00 + its $1.00 reservation > $2.00) throws.
func TestBudgetCapsRun(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "bud", 1, costLeaf(rec, 1.0))
	eng.budgetTotal = 2.0
	_, err := eng.run("b.js", []byte(`
for (let i = 0; i < 10; i++) {
    await agent("x" + i, {provider: "v"});
}
return {};
`), Options{})
	if err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("expected a budget-exceeded error, got %v", err)
	}
	if n := len(rec.prompts()); n != 2 {
		t.Errorf("ran %d leaves, want 2 (cap $2.00 at $1.00/leaf)", n)
	}
}

// TestBudgetReservationBoundsConcurrentOvershoot: a parallel() fan-out under a cap races the gate
// while spent is still ~0 (charges land only at completion). Admission is against spent+reserved, so
// the real total spend never exceeds the cap by more than ONE leaf's estimate; a gate-refused thunk
// degrades to null and the run still fulfills.
func TestBudgetReservationBoundsConcurrentOvershoot(t *testing.T) {
	rec := &recorder{}
	// Each leaf costs $1.00 — exactly its reservation, so spent tracks reserved with no slack.
	eng := budgetEngine(t, "bres", 8, costLeaf(rec, 1.0))
	eng.budgetTotal = 5.0
	if _, err := eng.run("b.js", []byte(`
const thunks = [];
for (let i = 0; i < 20; i++) {
    const j = i;
    thunks.push(() => agent("x" + j, {provider: "v"}));
}
await parallel(thunks);
return {};
`), Options{}); err != nil {
		t.Fatalf("run: %v (a gate-refused thunk degrades to null, not a run failure)", err)
	}
	if spent := eng.budgetSpent; spent > 5.0+1.0 { // cap + one leaf's estimate
		t.Errorf("real spend $%.2f overshot the $5.00 cap by more than one leaf's $1.00 estimate", spent)
	}
	if eng.budgetReserved != 0 {
		t.Errorf("every reservation must be released; budgetReserved = %v, want 0", eng.budgetReserved)
	}
}

// TestBudgetSpentRemainingTotal: spent()/remaining()/total reflect accumulated CostUSD.
func TestBudgetSpentRemainingTotal(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "bud2", 1, costLeaf(rec, 0.25))
	eng.budgetTotal = 2.0
	v, err := eng.run("b.js", []byte(`
await agent("a", {provider: "v"});
await agent("b", {provider: "v"});
return { sp: budget.spent(), rem: budget.remaining(), tot: budget.total };
`), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if f := budgetFloat(t, m, "sp"); f != 0.5 {
		t.Errorf("spent = %v, want 0.5", f)
	}
	if f := budgetFloat(t, m, "rem"); f != 1.5 {
		t.Errorf("remaining = %v, want 1.5", f)
	}
	if f := budgetFloat(t, m, "tot"); f != 2.0 {
		t.Errorf("total = %v, want 2.0", f)
	}
}

// TestBudgetUncapped: with no cap, total is null and remaining() is +Inf — so a
// `while (budget.remaining() > X)` loop is unbounded by budget (only the lifetime cap).
func TestBudgetUncapped(t *testing.T) {
	rec := &recorder{}
	eng := budgetEngine(t, "bud3", 1, costLeaf(rec, 0.1)) // budgetTotal 0 = uncapped
	v, err := eng.run("b.js", []byte(`
await agent("a", {provider: "v"});
return { tot: budget.total, rem: budget.remaining() };
`), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	m := wantMap(t, v)
	if tv, ok := m["tot"]; !ok || tv != nil {
		t.Errorf("uncapped total = %v, want null", tv)
	}
	if rem, ok := m["rem"].(float64); !ok || !math.IsInf(rem, 1) {
		t.Errorf("uncapped remaining = %v, want +Inf", m["rem"])
	}
}
