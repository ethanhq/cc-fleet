package workflow

import (
	"fmt"
	"math"

	"go.starlark.net/starlark"
)

// budgetValue is the predeclared `budget` object, mirroring native: budget.total
// (Float, or None when no cap is set), budget.spent() (Float, USD spent so far), and
// budget.remaining() (Float; +Inf when uncapped). It is a thin view over the engine's
// GIL-protected accounting (budgetTotal / budgetSpent), so every read happens
// single-threaded under the GIL — no separate lock. agent() enforces the cap (raises
// once spent >= total before launching a new leaf), and accumulates each completed
// leaf's CostUSD; a journal cache hit costs nothing.
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

func (b budgetValue) AttrNames() []string { return []string{"remaining", "spent", "total"} }

// Attr returns budget.total (Float|None) or the spent()/remaining() methods. A method
// reads the engine's budget fields under the GIL (all Starlark runs under it), so the
// value is consistent even when called from a parallel/pipeline goroutine.
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
	}
	return nil, nil // unknown attribute → Starlark reports "budget has no .<name>"
}
