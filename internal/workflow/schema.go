package workflow

import (
	"fmt"
	"math"
	"strings"

	"go.starlark.net/lib/json"
	"go.starlark.net/starlark"
)

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

// encodeSchema serializes a Starlark schema value to a JSON string — appended to the
// prompt AND folded into the journal key, so its output must be byte-stable
// (go.starlark.net's json.encode canonicalizes, so it is). Runs under the GIL.
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
// the schema. Runs under the GIL. A parse failure or any validation failure is an error
// the caller retries on (and, on resume, treats a cached value that no longer validates
// as a miss). The supported JSON-Schema subset is deliberately the practical core —
// `type` (object/array/string/number/integer/boolean/null), `required`, nested
// `properties`, array `items`, and scalar `enum`; composition keywords ($ref / allOf /
// oneOf / anyOf) are intentionally NOT enforced (over-scope for a vendor leaf that can't
// be force-terminated anyway), so a schema using only them validates as "valid JSON".
func decodeAndValidate(thread *starlark.Thread, reply string, schema starlark.Value) (starlark.Value, error) {
	v, err := starlark.Call(thread, jsonDecode, starlark.Tuple{starlark.String(stripCodeFence(reply))}, nil)
	if err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %v", err)
	}
	if err := validateAgainstSchema(v, schema, 0); err != nil {
		return nil, err
	}
	return v, nil
}

// validateAgainstSchema recursively checks value against schema (a *starlark.Dict). A
// non-dict schema imposes no structural constraint (valid-JSON-only). Errors are wrapped
// with the failing path (property/item) for an actionable retry message.
func validateAgainstSchema(value, schema starlark.Value, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("schema nesting exceeds %d levels", maxSchemaDepth)
	}
	sd, ok := schema.(*starlark.Dict)
	if !ok {
		return nil
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
				if err := validateAgainstSchema(cv, sub, depth+1); err != nil {
					ks, _ := starlark.AsString(k)
					return fmt.Errorf("property %q: %w", ks, err)
				}
			}
		}
	}
	if lst, ok := value.(*starlark.List); ok {
		if iv, found, _ := sd.Get(starlark.String("items")); found {
			for i := 0; i < lst.Len(); i++ {
				if err := validateAgainstSchema(lst.Index(i), iv, depth+1); err != nil {
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

// stripCodeFence removes a leading/trailing markdown code fence (```json … ```), a
// common vendor habit despite the prompt's "no fences" instruction, so the JSON
// inside still decodes. Leaves un-fenced replies untouched.
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
