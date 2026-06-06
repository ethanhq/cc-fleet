package workflow

import (
	"fmt"
	"math"
	"strings"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// defaultLeafEstimate / defaultLeafTokenEstimate are the per-leaf pessimistic RESERVATION an
// in-flight leaf holds against its budget cap until it reconciles to its real cost. Each is a
// deliberate over-estimate of a typical leaf, so a concurrent fan-out can't admit the whole batch
// against a near-zero spent and overshoot the cap by the in-flight set; reconcile-to-real frees the
// estimate the moment a leaf finishes, so they are tunable pessimism floors, not hard ceilings (a
// single leaf whose real cost exceeds its estimate still overshoots by that bounded error).
const (
	defaultLeafEstimate      = 1.0    // USD per leaf (a leaf's own max_budget_usd wins when larger)
	defaultLeafTokenEstimate = 50_000 // tokens per leaf
)

// budgetWouldExceed reports whether reserving (usd, tok) more would breach EITHER active cap —
// the first-to-trip-aborts gate. An unset cap (total<=0) never trips. GIL-held callers only.
func (e *engine) budgetWouldExceed(usd float64, tok int64) bool {
	if e.budgetTotal > 0 && e.budgetSpent+e.budgetReserved+usd > e.budgetTotal {
		return true
	}
	if e.budgetTokensTotal > 0 && e.budgetTokensSpent+e.budgetTokensReserved+tok > e.budgetTokensTotal {
		return true
	}
	return false
}

// budgetReserve / budgetRelease move a leaf's pessimistic estimate in/out of *Reserved; budgetCharge
// books its reconciled real cost into *Spent. This is the SINGLE reservation mechanism both caps
// share (USD + tokens). GIL-held callers only, so the counters stay exact across a parallel fan-out.
func (e *engine) budgetReserve(usd float64, tok int64) {
	e.budgetReserved += usd
	e.budgetTokensReserved += tok
}

func (e *engine) budgetRelease(usd float64, tok int64) {
	if e.budgetReserved -= usd; e.budgetReserved < 0 {
		e.budgetReserved = 0
	}
	if e.budgetTokensReserved -= tok; e.budgetTokensReserved < 0 {
		e.budgetTokensReserved = 0
	}
}

func (e *engine) budgetCharge(usd float64, tok int64) {
	e.budgetSpent += usd
	e.budgetTokensSpent += tok
}

// budgetExceededErr is the gate's refusal, naming only the active cap(s): USD (a list-price
// estimate) and/or tokens (exact). GIL-held caller.
func (e *engine) budgetExceededErr() error {
	var parts []string
	if e.budgetTotal > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f of $%.2f (list-price estimate)", e.budgetSpent+e.budgetReserved, e.budgetTotal))
	}
	if e.budgetTokensTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d tokens", e.budgetTokensSpent+e.budgetTokensReserved, e.budgetTokensTotal))
	}
	return fmt.Errorf("agent: budget exceeded — %s", strings.Join(parts, "; "))
}

// leafTokens is a completed leaf's token spend: input + output (the growing context plus the
// generated text), EXCLUDING cache-read — the exact, vendor-neutral unit --budget-tokens caps.
func leafTokens(res subagent.Result) int64 {
	if res.Usage == nil {
		return 0
	}
	return int64(res.Usage.InputTokens + res.Usage.OutputTokens)
}

// budgetValue is the predeclared `budget` object, mirroring native: budget.total / .spent() /
// .remaining() report the USD cap (a Float, or None/+Inf when uncapped) and tokens_total /
// tokens_spent() / tokens_remaining() report the token cap (an Int, or None/a large int when
// uncapped). The USD figure is an Anthropic LIST-PRICE estimate (claude's own metering, not the
// third-party vendor's actual charge); the token figure (input+output, cache-read excluded) is the
// exact vendor-neutral count. It is a thin view over the engine's GIL-protected accounting, so every
// read happens single-threaded under the GIL — no separate lock. agent() enforces the caps (the
// first to trip aborts) and books each completed leaf's real cost; a journal cache hit costs nothing.
type budgetValue struct{ e *engine }

var (
	_ starlark.Value    = budgetValue{}
	_ starlark.HasAttrs = budgetValue{}
)

func (b budgetValue) String() string        { return "budget" }
func (b budgetValue) Type() string          { return "budget" }
func (b budgetValue) Freeze()               {}
func (b budgetValue) Truth() starlark.Bool  { return starlark.True }
func (b budgetValue) Hash() (uint32, error) { return 0, fmt.Errorf("budget is unhashable") }

func (b budgetValue) AttrNames() []string {
	return []string{"remaining", "spent", "tokens_remaining", "tokens_spent", "tokens_total", "total"}
}

// Attr returns budget.total / tokens_total (Float|Int|None) or the spent()/remaining() methods. A
// method reads the engine's budget fields under the GIL (all Starlark runs under it), so the value is
// consistent even when called from a parallel/pipeline goroutine. The token figures mirror the USD
// ones in Int: tokens_total is None when uncapped; tokens_remaining is MaxInt64 when uncapped (the
// Int analog of the USD +Inf, so a `while budget.tokens_remaining() > N` loop stays unbounded).
func (b budgetValue) Attr(name string) (starlark.Value, error) {
	switch name {
	case "total":
		if b.e.budgetTotal <= 0 {
			return starlark.None, nil // uncapped
		}
		return starlark.Float(b.e.budgetTotal), nil
	case "spent":
		return starlark.NewBuiltin("spent", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return starlark.Float(b.e.budgetSpent), nil
		}), nil
	case "remaining":
		return starlark.NewBuiltin("remaining", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			if b.e.budgetTotal <= 0 {
				return starlark.Float(math.Inf(1)), nil
			}
			return starlark.Float(b.e.budgetTotal - b.e.budgetSpent), nil
		}), nil
	case "tokens_total":
		if b.e.budgetTokensTotal <= 0 {
			return starlark.None, nil // uncapped
		}
		return starlark.MakeInt64(b.e.budgetTokensTotal), nil
	case "tokens_spent":
		return starlark.NewBuiltin("tokens_spent", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			return starlark.MakeInt64(b.e.budgetTokensSpent), nil
		}), nil
	case "tokens_remaining":
		return starlark.NewBuiltin("tokens_remaining", func(*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple) (starlark.Value, error) {
			if b.e.budgetTokensTotal <= 0 {
				return starlark.MakeInt64(math.MaxInt64), nil
			}
			return starlark.MakeInt64(b.e.budgetTokensTotal - b.e.budgetTokensSpent), nil
		}), nil
	}
	return nil, nil // unknown attribute → Starlark reports "budget has no .<name>"
}
