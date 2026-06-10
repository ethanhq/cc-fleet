package workflow

import (
	"fmt"
	"regexp"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

// scriptMeta is the validated `const meta` declaration extracted from a script BEFORE
// execution, so the run manifest is minted with the name/description/phase plan up
// front — the board shows the named run + phase skeleton before the first leaf fires.
type scriptMeta struct {
	Name        string
	Description string
	// WhenToUse is optional display/board text (native's meta.whenToUse). Model is the
	// optional DEFAULT model for agents that omit model (native's meta.model); the engine
	// applies it as the fallback BEFORE computing the journal key, so the key reflects the
	// effective model consistently.
	WhenToUse string
	Model     string
	Phases    []phaseDecl
}

type phaseDecl struct {
	Title  string
	Detail string
}

// scriptPrefix/scriptSuffix wrap a script body as the async arrow the engine compiles
// and calls: top-level `await` and `return` are legal inside the wrapper, each script's
// top-level declarations are scoped to its own body (a nested child can't collide with
// its parent's), and `workflow`/`args` arrive as parameters (a nested child receives the
// one-level-guard stub and its own args). The prefix stays on line 1, so every parse and
// runtime position from line 2 on is the user's own; only line-1 columns shift.
const (
	scriptPrefix = "(async (workflow, args) => { "
	scriptSuffix = "\n})"
)

func wrapScript(src []byte) string { return scriptPrefix + string(src) + scriptSuffix }

var (
	// exportMetaRe matches the native `export const meta` form at the start of a line;
	// group 1 is exactly the export keyword normalizeScript blanks.
	exportMetaRe = regexp.MustCompile(`(?m)^[ \t]*(export)[ \t]+const[ \t]+meta\b`)
	// moduleSyntaxRe detects remaining ES-module syntax for the explicit unsupported error.
	moduleSyntaxRe = regexp.MustCompile(`(?m)^[ \t]*(import|export)\b`)
)

// parseWrapped parses (without executing) the wrapped form of a script body.
func parseWrapped(filename string, src []byte) (*ast.Program, error) {
	return parser.ParseFile(nil, filename, wrapScript(src), 0)
}

// normalizeScript returns the executable script body, accepting the native
// `export const meta` prefix: a script that parses as-is runs verbatim; only on a parse
// failure is the one export keyword blanked with same-width spaces (so every later
// error position stays true) and the parse retried. When the retry fails too, remaining
// module syntax (import / another export) gets the explicit unsupported-ES-modules
// error; anything else reports the original parse error.
func normalizeScript(filename string, src []byte) ([]byte, *ast.Program, error) {
	prog, err := parseWrapped(filename, src)
	if err == nil {
		return src, prog, nil
	}
	checkSrc := src
	if m := exportMetaRe.FindSubmatchIndex(src); m != nil {
		stripped := append([]byte(nil), src...)
		for i := m[2]; i < m[3]; i++ {
			stripped[i] = ' '
		}
		if prog2, err2 := parseWrapped(filename, stripped); err2 == nil {
			return stripped, prog2, nil
		}
		checkSrc = stripped
	}
	if moduleSyntaxRe.Match(checkSrc) {
		return nil, nil, fmt.Errorf("workflow: ES modules (import/export) are not supported — declare a plain `const meta = {...}` and use script statements only")
	}
	return nil, nil, err
}

// extractMeta finds the script's top-level `const meta = {…}` and evaluates it as a
// PURE LITERAL — an object with literal keys over strings / numbers / booleans / null /
// arrays / nested objects; any non-literal (a call, a name reference, an expression, a
// computed key, a spread) is rejected. That rejection is exactly what enforces native's
// "meta is a pure literal" rule, and it keeps the evaluator small. name and description
// are required non-empty strings.
func extractMeta(prog *ast.Program) (scriptMeta, error) {
	var lit *ast.ObjectLiteral
	for _, stmt := range wrappedBody(prog) {
		ld, ok := stmt.(*ast.LexicalDeclaration)
		if !ok || ld.Token != token.CONST {
			continue
		}
		for _, b := range ld.List {
			id, ok := b.Target.(*ast.Identifier)
			if !ok || id.Name.String() != "meta" {
				continue
			}
			ol, ok := b.Initializer.(*ast.ObjectLiteral)
			if !ok {
				return scriptMeta{}, fmt.Errorf("workflow: meta must be a pure literal: expected an object literal")
			}
			lit = ol
		}
		if lit != nil {
			break
		}
	}
	if lit == nil {
		return scriptMeta{}, fmt.Errorf("workflow: script has no top-level `const meta = {...}` declaration")
	}
	v, err := foldLiteral(lit)
	if err != nil {
		return scriptMeta{}, fmt.Errorf("workflow: meta must be a pure literal: %w", err)
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return scriptMeta{}, fmt.Errorf("workflow: meta must be an object")
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
	if wt, ok := m["whenToUse"].(string); ok {
		out.WhenToUse = wt
	}
	if md, ok := m["model"].(string); ok {
		out.Model = md
	}
	if praw, ok := m["phases"]; ok {
		plist, ok := praw.([]interface{})
		if !ok {
			return scriptMeta{}, fmt.Errorf("workflow: meta.phases must be an array of objects")
		}
		for _, p := range plist {
			pm, ok := p.(map[string]interface{})
			if !ok {
				return scriptMeta{}, fmt.Errorf("workflow: each meta.phases entry must be an object")
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

// wrappedBody returns the statement list of the wrapper arrow's block — the script's
// own top level. The engine built the wrapper itself, so a miss means the program is
// not a wrapped script at all (callers treat that as no-meta).
func wrappedBody(prog *ast.Program) []ast.Statement {
	if len(prog.Body) == 0 {
		return nil
	}
	es, ok := prog.Body[0].(*ast.ExpressionStatement)
	if !ok {
		return nil
	}
	arrow, ok := es.Expression.(*ast.ArrowFunctionLiteral)
	if !ok {
		return nil
	}
	blk, ok := arrow.Body.(*ast.BlockStatement)
	if !ok {
		return nil
	}
	return blk.List
}

// foldLiteral evaluates the pure-literal subset of JS expressions to Go values. A
// negative number parses as a unary minus over a number literal, so it is special-
// cased; every other expression node is rejected (the pure-literal enforcement).
func foldLiteral(e ast.Expression) (interface{}, error) {
	switch n := e.(type) {
	case *ast.StringLiteral:
		return n.Value.String(), nil
	case *ast.NumberLiteral:
		switch v := n.Value.(type) {
		case int64:
			return v, nil
		case float64:
			return v, nil
		case int:
			return int64(v), nil
		}
		return nil, fmt.Errorf("unsupported number literal")
	case *ast.BooleanLiteral:
		return n.Value, nil
	case *ast.NullLiteral:
		return nil, nil
	case *ast.UnaryExpression:
		if n.Operator == token.MINUS {
			x, err := foldLiteral(n.Operand)
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
	case *ast.ArrayLiteral:
		out := make([]interface{}, 0, len(n.Value))
		for _, el := range n.Value {
			v, err := foldLiteral(el)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case *ast.ObjectLiteral:
		out := make(map[string]interface{}, len(n.Value))
		for _, prop := range n.Value {
			kv, ok := prop.(*ast.PropertyKeyed)
			if !ok || kv.Kind != ast.PropertyKindValue || kv.Computed {
				return nil, fmt.Errorf("meta supports only plain `key: value` entries")
			}
			var key string
			switch k := kv.Key.(type) {
			case *ast.StringLiteral:
				key = k.Value.String()
			case *ast.Identifier:
				key = k.Name.String()
			default:
				return nil, fmt.Errorf("meta object keys must be identifiers or string literals")
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("duplicate key %q in meta literal", key)
			}
			val, err := foldLiteral(kv.Value)
			if err != nil {
				return nil, err
			}
			out[key] = val
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported expression in meta literal (only object/array/string/number/boolean/null)")
}
