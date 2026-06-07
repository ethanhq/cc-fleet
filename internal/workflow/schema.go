package workflow

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
)

// schemaError marks a structural schema defect — an unresolvable or malformed `$ref`, or nesting
// beyond maxSchemaDepth — distinct from a value/schema MISMATCH. anyOf/oneOf must PROPAGATE it (a
// broken branch is not "the value didn't match this branch"), never count it as a non-match.
type schemaError struct{ err error }

func (e schemaError) Error() string { return e.err.Error() }

func isSchemaError(err error) bool {
	var se schemaError
	return errors.As(err, &se)
}

// jsonEncode / jsonDecode reuse go.starlark.net's tested JSON module instead of a
// bespoke Go<->Starlark converter. Both are called via starlark.Call UNDER THE GIL
// (like any Starlark op), so they participate in the single-interpreter discipline.
var (
	jsonEncode = json.Module.Members["encode"]
	jsonDecode = json.Module.Members["decode"]
)

// maxSchemaDepth bounds recursive schema validation against a pathological deeply-nested
// schema (and a deeply-nested reply). Real schemas are shallow; this is a backstop.
const maxSchemaDepth = 32

// encodeSchema serializes a Starlark schema value to a JSON string — passed to the
// leaf via --json-schema AND folded into the journal key, so its output must be
// byte-stable (go.starlark.net's json.encode canonicalizes, so it is). Runs under the GIL.
func encodeSchema(thread *starlark.Thread, schema starlark.Value) (string, error) {
	enc, err := starlark.Call(thread, jsonEncode, starlark.Tuple{schema}, nil)
	if err != nil {
		return "", err
	}
	s, ok := starlark.AsString(enc)
	if !ok {
		return "", fmt.Errorf("json.encode did not return a string")
	}
	return s, nil
}

// decodeAndValidate parses the vendor reply as JSON and recursively validates it against
// the schema. Runs under the GIL. A parse failure or any validation failure is an error —
// terminal for a live leaf, and on resume a cached value that no longer validates is
// treated as a miss. The supported keywords are `type` (object/array/string/number/integer/
// boolean/null), `required`, nested `properties`, array `items`, scalar `enum`, string `pattern`
// (RE2 best-effort — an uncompilable ECMA pattern defers to the wire) and `format`, `additionalProperties`,
// the composition keywords `allOf` / `anyOf` / `oneOf`, and intra-document `$ref` (`#/…` JSON
// pointers into the root schema, e.g. `#/$defs/Address`). External `$ref` URIs are not
// resolved (surfaced as a validation error, not silently passed).
//
// This is a single-pass VALUE validator, not a value-independent schema linter: an unresolvable
// `$ref` always surfaces from a composition branch, but a structural defect buried behind a value
// mismatch in a deeply-nested non-composition keyword may not. Recursion (a deep or cyclic `$ref`)
// is bounded by `maxSchemaDepth`, so it terminates with a depth error rather than looping. It is
// the backstop; claude enforces the full schema on the wire via `--json-schema`.
func decodeAndValidate(thread *starlark.Thread, reply string, schema starlark.Value) (starlark.Value, error) {
	v, err := starlark.Call(thread, jsonDecode, starlark.Tuple{starlark.String(stripCodeFence(reply))}, nil)
	if err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %v", err)
	}
	if err := validateAgainstSchema(v, schema, schema, 0); err != nil {
		return nil, err
	}
	return v, nil
}

// validateAgainstSchema recursively checks value against schema (a *starlark.Dict). A
// non-dict schema imposes no structural constraint (valid-JSON-only). Errors are wrapped
// with the failing path (property/item) for an actionable retry message.
func validateAgainstSchema(value, schema, root starlark.Value, depth int) error {
	if depth > maxSchemaDepth {
		return schemaError{fmt.Errorf("schema nesting exceeds %d levels", maxSchemaDepth)}
	}
	sd, ok := schema.(*starlark.Dict)
	if !ok {
		return nil
	}
	// Resolve $ref + composition FIRST, so a structural defect (an unresolvable $ref, nesting too
	// deep) surfaces as a schemaError even when a sibling keyword (e.g. a mismatching type) would
	// otherwise short-circuit with a plain value mismatch — anyOf/oneOf must see the defect.
	if err := checkComposition(value, sd, root, depth); err != nil {
		return err
	}
	if tv, found, _ := sd.Get(starlark.String("type")); found {
		if ts, ok := starlark.AsString(tv); ok {
			if err := checkType(value, ts); err != nil {
				return err
			}
		}
	}
	if ev, found, _ := sd.Get(starlark.String("enum")); found {
		if lst, ok := ev.(*starlark.List); ok && !enumContains(lst, value) {
			return fmt.Errorf("value %s is not one of the enum values", value.String())
		}
	}
	// pattern / format constrain STRING values only (a non-string is left to `type`).
	if s, isStr := starlark.AsString(value); isStr {
		if pv, found, _ := sd.Get(starlark.String("pattern")); found {
			if pat, ok := starlark.AsString(pv); ok {
				// pattern is ECMA-262 in JSON Schema; Go RE2 only approximates it. An uncompilable
				// (ECMA-only) pattern is skipped; the rare RE2-vs-ECMA semantic divergence (an escape
				// RE2 reads differently) is an accepted best-effort caveat — claude enforces the
				// authoritative pattern on the wire via --json-schema, and this local check is a backstop
				// (chiefly for resume re-validation), so a stray mismatch only re-runs a leaf, never wrong.
				if re, cerr := regexp.Compile(pat); cerr == nil && !re.MatchString(s) {
					return fmt.Errorf("value does not match pattern %q", pat)
				}
			}
		}
		if fv, found, _ := sd.Get(starlark.String("format")); found {
			if format, ok := starlark.AsString(fv); ok {
				if err := checkFormat(format, s); err != nil {
					return err
				}
			}
		}
	}
	rv, hasRequired, _ := sd.Get(starlark.String("required"))
	pv, hasProperties, _ := sd.Get(starlark.String("properties"))
	d, isObject := value.(*starlark.Dict)
	// `required`/`properties` are object constraints — a non-object reply (e.g. the bare
	// string "oops" for schema={"required":["answer"]}) must FAIL, not slip through.
	if (hasRequired || hasProperties) && !isObject {
		return fmt.Errorf("expected a JSON object, got %s", value.Type())
	}
	if isObject {
		if lst, ok := rv.(*starlark.List); ok {
			for i := 0; i < lst.Len(); i++ {
				ks, ok := starlark.AsString(lst.Index(i))
				if !ok {
					continue
				}
				if _, f, _ := d.Get(starlark.String(ks)); !f {
					return fmt.Errorf("missing required key %q", ks)
				}
			}
		}
		if props, ok := pv.(*starlark.Dict); ok {
			for _, k := range props.Keys() {
				cv, present, _ := d.Get(k)
				if !present {
					continue // properties does NOT imply required (JSON-Schema semantics)
				}
				sub, _, _ := props.Get(k)
				if err := validateAgainstSchema(cv, sub, root, depth+1); err != nil {
					ks, _ := starlark.AsString(k)
					return fmt.Errorf("property %q: %w", ks, err)
				}
			}
		}
		// additionalProperties governs keys NOT named in `properties`: `false` rejects any extra
		// key; a schema validates each extra value against it; `true`/absent imposes nothing.
		if apv, found, _ := sd.Get(starlark.String("additionalProperties")); found {
			if err := checkAdditionalProps(d, pv, apv, root, depth); err != nil {
				return err
			}
		}
	}
	if lst, ok := value.(*starlark.List); ok {
		if iv, found, _ := sd.Get(starlark.String("items")); found {
			for i := 0; i < lst.Len(); i++ {
				if err := validateAgainstSchema(lst.Index(i), iv, root, depth+1); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	}
	return nil
}

// checkType verifies value's JSON type. `integer` accepts a zero-fraction Float (a vendor
// may emit 5.0 for an integer field); an unknown type name imposes no constraint.
func checkType(v starlark.Value, t string) error {
	ok := true
	switch t {
	case "object":
		_, ok = v.(*starlark.Dict)
	case "array":
		_, ok = v.(*starlark.List)
	case "string":
		_, ok = v.(starlark.String)
	case "boolean":
		_, ok = v.(starlark.Bool)
	case "null":
		ok = v == starlark.None
	case "number":
		ok = isJSONNumber(v)
	case "integer":
		ok = isJSONInteger(v)
	}
	if !ok {
		return fmt.Errorf("expected type %s, got %s", t, v.Type())
	}
	return nil
}

func isJSONNumber(v starlark.Value) bool {
	switch v.(type) {
	case starlark.Int, starlark.Float:
		return true
	}
	return false
}

func isJSONInteger(v starlark.Value) bool {
	switch n := v.(type) {
	case starlark.Int:
		return true
	case starlark.Float:
		f := float64(n)
		return !math.IsInf(f, 0) && f == math.Trunc(f)
	}
	return false
}

// enumContains reports whether v equals any element of lst (Starlark value equality).
func enumContains(lst *starlark.List, v starlark.Value) bool {
	for i := 0; i < lst.Len(); i++ {
		if eq, err := starlark.Equal(lst.Index(i), v); err == nil && eq {
			return true
		}
	}
	return false
}

// stripCodeFence removes a leading/trailing markdown code fence (```json … ```) so
// fenced JSON still decodes: a structured_output payload never carries fences, but
// pre-v2 journals cached raw text answers that may. Leaves un-fenced replies untouched.
func stripCodeFence(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return t
	}
	t = strings.TrimPrefix(t, "```")
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:] // drop the ```/```json info line
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// checkComposition enforces the schema-composition keywords on value: $ref (resolve a local
// JSON pointer into root, then validate against the target), allOf (every subschema must
// pass), anyOf (at least one), oneOf (exactly one). Each is independent and ANDed with the
// rest of the schema; an absent keyword is a no-op.
func checkComposition(value starlark.Value, sd *starlark.Dict, root starlark.Value, depth int) error {
	if rv, found, _ := sd.Get(starlark.String("$ref")); found {
		ref, ok := starlark.AsString(rv)
		if !ok {
			return schemaError{fmt.Errorf("$ref must be a string")}
		}
		target, rerr := resolveRef(ref, root)
		if rerr != nil {
			return schemaError{rerr} // an unresolvable ref is a schema defect, not a value mismatch
		}
		if err := validateAgainstSchema(value, target, root, depth+1); err != nil {
			return fmt.Errorf("$ref %s: %w", ref, err)
		}
	}
	if av, found, _ := sd.Get(starlark.String("allOf")); found {
		if lst, ok := av.(*starlark.List); ok {
			var firstMismatch error
			for i := 0; i < lst.Len(); i++ {
				err := validateAgainstSchema(value, lst.Index(i), root, depth+1)
				if err == nil {
					continue
				}
				if isSchemaError(err) {
					return err // a broken branch — surface ahead of any value mismatch (scan all branches)
				}
				if firstMismatch == nil {
					firstMismatch = fmt.Errorf("allOf[%d]: %w", i, err)
				}
			}
			if firstMismatch != nil {
				return firstMismatch // allOf requires every branch; report the first value mismatch
			}
		}
	}
	if av, found, _ := sd.Get(starlark.String("anyOf")); found {
		if lst, ok := av.(*starlark.List); ok { // an empty anyOf matches nothing → fails below
			matched := false
			for i := 0; i < lst.Len(); i++ {
				err := validateAgainstSchema(value, lst.Index(i), root, depth+1)
				if err == nil {
					matched = true
					continue // keep scanning — a LATER branch may be structurally broken
				}
				if isSchemaError(err) {
					return err // a broken branch (unresolvable $ref / too deep) — surface, don't swallow
				}
			}
			if !matched {
				return fmt.Errorf("anyOf: value matches none of the %d subschemas", lst.Len())
			}
		}
	}
	if ov, found, _ := sd.Get(starlark.String("oneOf")); found {
		if lst, ok := ov.(*starlark.List); ok { // an empty oneOf matches 0 subschemas → fails below
			matched := 0
			for i := 0; i < lst.Len(); i++ {
				err := validateAgainstSchema(value, lst.Index(i), root, depth+1)
				if err == nil {
					matched++
					continue
				}
				if isSchemaError(err) {
					return err // a broken branch — surface, don't swallow as a non-match
				}
			}
			if matched != 1 {
				return fmt.Errorf("oneOf: value matches %d subschemas, want exactly 1", matched)
			}
		}
	}
	return nil
}

// resolveRef resolves an intra-document JSON-pointer $ref into root and returns the referenced
// subschema. Only intra-document pointers (`#` and `#/…`) are supported; an external URI is an
// error so an unresolvable ref FAILS validation rather than silently passing.
func resolveRef(ref string, root starlark.Value) (starlark.Value, error) {
	if ref == "#" {
		return root, nil
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, fmt.Errorf("unsupported $ref %q (only intra-document #/… pointers)", ref)
	}
	cur := root
	for _, tok := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		tok = unescapeJSONPointer(tok)
		switch c := cur.(type) {
		case *starlark.Dict:
			nv, found, _ := c.Get(starlark.String(tok))
			if !found {
				return nil, fmt.Errorf("$ref %q: %q not found", ref, tok)
			}
			cur = nv
		case *starlark.List:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= c.Len() {
				return nil, fmt.Errorf("$ref %q: %q is not a valid array index", ref, tok)
			}
			cur = c.Index(idx)
		default:
			return nil, fmt.Errorf("$ref %q: %q is not an object or array", ref, tok)
		}
	}
	return cur, nil
}

// unescapeJSONPointer decodes the RFC 6901 escapes ~1 → "/" then ~0 → "~" (order matters) so
// a key containing "/" or "~" resolves correctly.
func unescapeJSONPointer(t string) string {
	t = strings.ReplaceAll(t, "~1", "/")
	return strings.ReplaceAll(t, "~0", "~")
}

// checkAdditionalProps enforces additionalProperties on an object: for each key NOT named in
// `properties`, reject it when additionalProperties is `false`, or validate its value against the
// additionalProperties schema. `true` (or a non-bool, non-dict value) imposes nothing.
func checkAdditionalProps(d *starlark.Dict, properties, ap, root starlark.Value, depth int) error {
	declared := map[string]bool{}
	if props, ok := properties.(*starlark.Dict); ok {
		for _, k := range props.Keys() {
			if ks, ok := starlark.AsString(k); ok {
				declared[ks] = true
			}
		}
	}
	allowed := true
	var apSchema starlark.Value
	switch a := ap.(type) {
	case starlark.Bool:
		allowed = bool(a)
	case *starlark.Dict:
		apSchema = a
	}
	for _, k := range d.Keys() {
		ks, ok := starlark.AsString(k)
		if !ok || declared[ks] {
			continue
		}
		if !allowed {
			return fmt.Errorf("additional property %q is not allowed", ks)
		}
		if apSchema != nil {
			cv, _, _ := d.Get(k)
			if err := validateAgainstSchema(cv, apSchema, root, depth+1); err != nil {
				return fmt.Errorf("additional property %q: %w", ks, err)
			}
		}
	}
	return nil
}

var (
	formatEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	formatUUID  = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// checkFormat validates a string against a named `format`. Only the common set is enforced —
// email / uri (or url) / uuid / date / date-time; an unknown format imposes nothing (per JSON
// Schema, `format` is an annotation unless the validator opts in).
func checkFormat(format, s string) error {
	ok := true
	switch format {
	case "email":
		ok = formatEmail.MatchString(s)
	case "uri", "url":
		u, err := url.Parse(s)
		ok = err == nil && u.IsAbs()
	case "uuid":
		ok = formatUUID.MatchString(s)
	case "date":
		_, err := time.Parse("2006-01-02", s)
		ok = err == nil
	case "date-time":
		_, err := time.Parse(time.RFC3339, s)
		ok = err == nil
	default:
		return nil // unknown format: annotation, not a constraint
	}
	if !ok {
		return fmt.Errorf("value %q is not a valid %s", s, format)
	}
	return nil
}
