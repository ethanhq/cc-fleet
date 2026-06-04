package workflow

import (
	"fmt"
	"math/big"

	"go.starlark.net/syntax"
)

// scriptMeta is the validated `meta` declaration extracted from a script BEFORE
// execution, so the run manifest is minted with the name/description/phase plan up
// front — the board shows the named run + phase skeleton before the first leaf fires.
type scriptMeta struct {
	Name        string
	Description string
	Phases      []phaseDecl
}

type phaseDecl struct {
	Title  string
	Detail string
}

// extractMeta parses the script and evaluates its top-level `meta = {…}` literal
// WITHOUT running the body. meta must be a PURE LITERAL — a dict with string keys
// over strings / numbers / bools / None / lists / nested dicts; any non-literal (a
// call, a name reference, a binary op, a comprehension) is rejected. That rejection
// is exactly what enforces native's "meta is a pure literal" rule, and it keeps the
// evaluator small. name and description are required non-empty strings.
func extractMeta(opts *syntax.FileOptions, filename string, src interface{}) (scriptMeta, error) {
	f, err := opts.Parse(filename, src, 0)
	if err != nil {
		return scriptMeta{}, err
	}
	var rhs syntax.Expr
	for _, stmt := range f.Stmts {
		as, ok := stmt.(*syntax.AssignStmt)
		if !ok || as.Op != syntax.EQ {
			continue
		}
		if id, ok := as.LHS.(*syntax.Ident); ok && id.Name == "meta" {
			rhs = as.RHS // first wins; a second top-level meta= is rejected at exec (GlobalReassign off)
			break
		}
	}
	if rhs == nil {
		return scriptMeta{}, fmt.Errorf("workflow: script has no top-level `meta = {...}` declaration")
	}
	v, err := evalLiteral(rhs)
	if err != nil {
		return scriptMeta{}, fmt.Errorf("workflow: meta must be a pure literal: %w", err)
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return scriptMeta{}, fmt.Errorf("workflow: meta must be a dict")
	}
	name, _ := m["name"].(string)
	if name == "" {
		return scriptMeta{}, fmt.Errorf("workflow: meta.name is required (a non-empty string)")
	}
	desc, _ := m["description"].(string)
	if desc == "" {
		return scriptMeta{}, fmt.Errorf("workflow: meta.description is required (a non-empty string)")
	}
	out := scriptMeta{Name: name, Description: desc}
	if praw, ok := m["phases"]; ok {
		plist, ok := praw.([]interface{})
		if !ok {
			return scriptMeta{}, fmt.Errorf("workflow: meta.phases must be a list of dicts")
		}
		for _, p := range plist {
			pm, ok := p.(map[string]interface{})
			if !ok {
				return scriptMeta{}, fmt.Errorf("workflow: each meta.phases entry must be a dict")
			}
			title, _ := pm["title"].(string)
			if title == "" {
				return scriptMeta{}, fmt.Errorf("workflow: each meta.phases entry needs a non-empty string title")
			}
			detail, _ := pm["detail"].(string)
			out.Phases = append(out.Phases, phaseDecl{Title: title, Detail: detail})
		}
	}
	return out, nil
}

// evalLiteral evaluates the pure-literal subset of Starlark to Go values. Note that
// True/False/None are NOT literal tokens — they parse as *Ident — and a negative
// number is a *UnaryExpr(MINUS) over a numeric *Literal, so both are special-cased;
// every other expression node is rejected (which is the pure-literal enforcement).
func evalLiteral(e syntax.Expr) (interface{}, error) {
	switch n := e.(type) {
	case *syntax.ParenExpr:
		return evalLiteral(n.X)
	case *syntax.Literal:
		switch v := n.Value.(type) {
		case string:
			return v, nil
		case int64:
			return v, nil
		case int:
			return int64(v), nil
		case *big.Int:
			return v.String(), nil // arbitrary-precision int → string (meta never needs huge ints)
		case float64:
			return v, nil
		}
		return nil, fmt.Errorf("unsupported literal token %v", n.Token)
	case *syntax.Ident:
		switch n.Name {
		case "True":
			return true, nil
		case "False":
			return false, nil
		case "None":
			return nil, nil
		}
		return nil, fmt.Errorf("name %q is not a literal (meta must be a constant)", n.Name)
	case *syntax.UnaryExpr:
		if n.Op == syntax.MINUS && n.X != nil {
			x, err := evalLiteral(n.X)
			if err != nil {
				return nil, err
			}
			switch v := x.(type) {
			case int64:
				return -v, nil
			case float64:
				return -v, nil
			}
		}
		return nil, fmt.Errorf("unsupported unary expression in meta literal")
	case *syntax.ListExpr:
		out := make([]interface{}, 0, len(n.List))
		for _, el := range n.List {
			v, err := evalLiteral(el)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case *syntax.DictExpr:
		out := make(map[string]interface{}, len(n.List))
		for _, item := range n.List {
			entry, ok := item.(*syntax.DictEntry)
			if !ok {
				return nil, fmt.Errorf("malformed dict in meta literal")
			}
			kv, err := evalLiteral(entry.Key)
			if err != nil {
				return nil, err
			}
			key, ok := kv.(string)
			if !ok {
				return nil, fmt.Errorf("meta dict keys must be string literals")
			}
			if _, dup := out[key]; dup {
				// Match Starlark's runtime MAKEDICT, which rejects duplicate keys, so
				// Prepare fails before minting rather than minting a manifest the body
				// would then fail to compile.
				return nil, fmt.Errorf("duplicate key %q in meta literal", key)
			}
			val, err := evalLiteral(entry.Value)
			if err != nil {
				return nil, err
			}
			out[key] = val
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported expression in meta literal (only dict/list/string/number/bool/None)")
}
