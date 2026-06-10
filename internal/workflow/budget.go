package workflow

import (
	"fmt"
	"math"
	"strings"

	"github.com/dop251/goja"

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
// the first-to-trip-aborts gate. An unset cap (total<=0) never trips. Loop-held callers only.
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
// share (USD + tokens). Loop-held callers only, so the counters stay exact across a parallel fan-out.
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
	// Persist the new running spend so an external `workflow status` reader sees a live run-level
	// total mid-run (the manifest otherwise only restamps at phase/terminal transitions). Bounded
	// by real leaf completions (cache hits never charge); loop-held, so it serializes with charging.
	e.saveManifest("running", "")
}

// budgetExceededErr is the gate's refusal, naming only the active cap(s): USD (a list-price
// estimate) and/or tokens (exact). Loop-held caller.
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

// newBudgetObject builds the script-facing `budget` object, mirroring native: total /
// spent() / remaining() report the USD cap (null when uncapped; remaining → +Inf) and
// tokens_total / tokens_spent() / tokens_remaining() the token cap (null / MaxInt64 when
// uncapped, so a `while remaining() > N` loop stays unbounded). The USD figure is an
// Anthropic LIST-PRICE estimate (claude's own metering, not the third-party vendor's
// actual charge); the token figure (input+output, cache-read excluded) is the exact
// vendor-neutral count. Every function executes on the engine loop — the only place JS
// runs — so reads are consistent with the gates. Properties are non-writable so a
// script can't clobber the accounting view.
func newBudgetObject(vm *goja.Runtime, e *engine) *goja.Object {
	obj := vm.NewObject()
	def := func(name string, v goja.Value) {
		_ = obj.DefineDataProperty(name, v, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	if e.budgetTotal > 0 {
		def("total", vm.ToValue(e.budgetTotal))
	} else {
		def("total", goja.Null())
	}
	def("spent", vm.ToValue(func(goja.FunctionCall) goja.Value {
		return vm.ToValue(e.budgetSpent)
	}))
	def("remaining", vm.ToValue(func(goja.FunctionCall) goja.Value {
		if e.budgetTotal <= 0 {
			return vm.ToValue(math.Inf(1))
		}
		return vm.ToValue(e.budgetTotal - e.budgetSpent)
	}))
	if e.budgetTokensTotal > 0 {
		def("tokens_total", vm.ToValue(e.budgetTokensTotal))
	} else {
		def("tokens_total", goja.Null())
	}
	def("tokens_spent", vm.ToValue(func(goja.FunctionCall) goja.Value {
		return vm.ToValue(e.budgetTokensSpent)
	}))
	def("tokens_remaining", vm.ToValue(func(goja.FunctionCall) goja.Value {
		if e.budgetTokensTotal <= 0 {
			return vm.ToValue(int64(math.MaxInt64))
		}
		return vm.ToValue(e.budgetTokensTotal - e.budgetTokensSpent)
	}))
	return obj
}
