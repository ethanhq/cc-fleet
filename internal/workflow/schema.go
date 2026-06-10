package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// schemaError marks a structural schema defect (a dangling/malformed `$ref` or nesting past
// maxSchemaDepth), distinct from a value mismatch. anyOf/oneOf propagate it rather than count it
// as a branch that did not match.
type schemaError struct{ err error }

func (e schemaError) Error() string { return e.err.Error() }

func isSchemaError(err error) bool {
	var se schemaError
	return errors.As(err, &se)
}

// maxSchemaDepth bounds recursive schema validation against a pathological deeply-nested
// schema (and a deeply-nested reply). Real schemas are shallow; this is a backstop.
const maxSchemaDepth = 32

// canonicalSchemaJSON serializes an agent() schema value to canonical JSON — passed to
// the leaf via --json-schema AND folded into the journal key, so it must be byte-stable
// and JS-faithful: the VM's own JSON.stringify defines the value semantics (undefined /
// function / symbol members drop, non-finite numbers become null, a cycle throws), then
// a Go decode/encode round-trip sorts the object keys so member insertion order can't
// shift the bytes. Depth-bounded like validation.
func (e *engine) canonicalSchemaJSON(v goja.Value) (string, error) {
	sv, err := e.jsonStringify(goja.Undefined(), v)
	if err != nil {
		return "", fmt.Errorf("schema is not JSON-serializable: %v", err)
	}
	s, ok := sv.Export().(string)
	if !ok {
		return "", fmt.Errorf("schema must be a JSON-serializable value")
	}
	var tree interface{}
	if err := json.Unmarshal([]byte(s), &tree); err != nil {
		return "", fmt.Errorf("schema is not valid JSON: %v", err)
	}
	if err := checkSchemaDepth(tree, 0); err != nil {
		return "", err
	}
	data, err := json.Marshal(tree)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// checkSchemaDepth bounds a schema tree's nesting (the stringify round-trip already
// guarantees the value is pure JSON).
func checkSchemaDepth(v interface{}, depth int) error {
	if depth > maxSchemaDepth {
		return schemaError{fmt.Errorf("schema nesting exceeds %d levels", maxSchemaDepth)}
	}
	switch t := v.(type) {
	case map[string]interface{}:
		for _, el := range t {
			if err := checkSchemaDepth(el, depth+1); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, el := range t {
			if err := checkSchemaDepth(el, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// decodeAndValidate parses the reply JSON and validates it against the schema subset: `type`,
// `required`, `properties`, `items`, `enum`, `pattern`/`format`/`additionalProperties`,
// `allOf`/`anyOf`/`oneOf`, and intra-document `$ref` (`#/…`; an external ref is unsupported, fails).
// It is the local backstop for claude's `--json-schema`: a live failure is terminal, a resume
// failure is a cache miss. Both sides decode through encoding/json, so every number is a float64
// and equality stays uniform. Single-pass over the value; `maxSchemaDepth` bounds a recursive `$ref`.
func decodeAndValidate(reply, schemaJSON string) (interface{}, error) {
	var v interface{}
	if err := json.Unmarshal([]byte(stripCodeFence(reply)), &v); err != nil {
		return nil, fmt.Errorf("reply is not valid JSON: %v", err)
	}
	var schema interface{}
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return nil, schemaError{fmt.Errorf("schema is not valid JSON: %v", err)}
	}
	if err := validateAgainstSchema(v, schema, schema, 0); err != nil {
		return nil, err
	}
	return v, nil
}

// validateAgainstSchema recursively checks value against schema (a JSON object). A
// non-object schema imposes no structural constraint (valid-JSON-only). Errors are
// wrapped with the failing path (property/item) for an actionable retry message.
func validateAgainstSchema(value, schema, root interface{}, depth int) error {
	if depth > maxSchemaDepth {
		return schemaError{fmt.Errorf("schema nesting exceeds %d levels", maxSchemaDepth)}
	}
	sd, ok := schema.(map[string]interface{})
	if !ok {
		return nil
	}
	// Resolve $ref + composition FIRST, so a structural defect (an unresolvable $ref, nesting too
	// deep) surfaces as a schemaError even when a sibling keyword (e.g. a mismatching type) would
	// otherwise short-circuit with a plain value mismatch — anyOf/oneOf must see the defect.
	if err := checkComposition(value, sd, root, depth); err != nil {
		return err
	}
	if ts, ok := sd["type"].(string); ok {
		if err := checkType(value, ts); err != nil {
			return err
		}
	}
	if lst, ok := sd["enum"].([]interface{}); ok && !enumContains(lst, value) {
		return fmt.Errorf("value %v is not one of the enum values", value)
	}
	// pattern / format constrain STRING values only (a non-string is left to `type`).
	if s, isStr := value.(string); isStr {
		if pat, ok := sd["pattern"].(string); ok {
			// JSON Schema `pattern` is ECMA-262; Go RE2 is a best-effort local approximation. An
			// uncompilable pattern is skipped — `--json-schema` stays authoritative on the wire.
			if re, cerr := regexp.Compile(pat); cerr == nil && !re.MatchString(s) {
				return fmt.Errorf("value does not match pattern %q", pat)
			}
		}
		if format, ok := sd["format"].(string); ok {
			if err := checkFormat(format, s); err != nil {
				return err
			}
		}
	}
	rv, hasRequired := sd["required"]
	pv, hasProperties := sd["properties"]
	d, isObject := value.(map[string]interface{})
	// `required`/`properties` are object constraints — a non-object reply (e.g. the bare
	// string "oops" for schema={"required":["answer"]}) must FAIL, not slip through.
	if (hasRequired || hasProperties) && !isObject {
		return fmt.Errorf("expected a JSON object, got %s", jsonTypeName(value))
	}
	if isObject {
		if lst, ok := rv.([]interface{}); ok {
			for _, k := range lst {
				ks, ok := k.(string)
				if !ok {
					continue
				}
				if _, f := d[ks]; !f {
					return fmt.Errorf("missing required key %q", ks)
				}
			}
		}
		if props, ok := pv.(map[string]interface{}); ok {
			for k, sub := range props {
				cv, present := d[k]
				if !present {
					continue // properties does NOT imply required (JSON-Schema semantics)
				}
				if err := validateAgainstSchema(cv, sub, root, depth+1); err != nil {
					return fmt.Errorf("property %q: %w", k, err)
				}
			}
		}
		// additionalProperties governs keys NOT named in `properties`: `false` rejects any extra
		// key; a schema validates each extra value against it; `true`/absent imposes nothing.
		if apv, found := sd["additionalProperties"]; found {
			if err := checkAdditionalProps(d, pv, apv, root, depth); err != nil {
				return err
			}
		}
	}
	if lst, ok := value.([]interface{}); ok {
		if iv, found := sd["items"]; found {
			for i, el := range lst {
				if err := validateAgainstSchema(el, iv, root, depth+1); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		}
	}
	return nil
}

// checkType verifies value's JSON type. `integer` accepts a zero-fraction number (a vendor
// may emit 5.0 for an integer field); an unknown type name imposes no constraint.
func checkType(v interface{}, t string) error {
	ok := true
	switch t {
	case "object":
		_, ok = v.(map[string]interface{})
	case "array":
		_, ok = v.([]interface{})
	case "string":
		_, ok = v.(string)
	case "boolean":
		_, ok = v.(bool)
	case "null":
		ok = v == nil
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && !math.IsInf(f, 0) && f == math.Trunc(f)
	}
	if !ok {
		return fmt.Errorf("expected type %s, got %s", t, jsonTypeName(v))
	}
	return nil
}

// jsonTypeName names a decoded JSON value's type for error messages.
func jsonTypeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// enumContains reports whether v equals any element of lst. Both sides came through
// encoding/json (uniform float64 numbers), so deep equality is exact.
func enumContains(lst []interface{}, v interface{}) bool {
	for _, el := range lst {
		if reflect.DeepEqual(el, v) {
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
func checkComposition(value interface{}, sd map[string]interface{}, root interface{}, depth int) error {
	if rv, found := sd["$ref"]; found {
		ref, ok := rv.(string)
		if !ok {
			return schemaError{fmt.Errorf("$ref must be a string")}
		}
		// Only an intra-document ref resolves locally; an external URI is unsupported and fails here
		// (claude's --json-schema is authoritative for it). A dangling intra-document ref is a defect.
		target, rerr := resolveRef(ref, root)
		if rerr != nil {
			return schemaError{rerr}
		}
		if err := validateAgainstSchema(value, target, root, depth+1); err != nil {
			return fmt.Errorf("$ref %s: %w", ref, err)
		}
	}
	if lst, ok := sd["allOf"].([]interface{}); ok {
		var firstMismatch error
		for i, sub := range lst {
			err := validateAgainstSchema(value, sub, root, depth+1)
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
	if lst, ok := sd["anyOf"].([]interface{}); ok { // an empty anyOf matches nothing → fails below
		matched := false
		for _, sub := range lst {
			err := validateAgainstSchema(value, sub, root, depth+1)
			if err == nil {
				matched = true
				continue // keep scanning — a LATER branch may be structurally broken
			}
			if isSchemaError(err) {
				return err // a broken branch (unresolvable $ref / too deep) — surface, don't swallow
			}
		}
		if !matched {
			return fmt.Errorf("anyOf: value matches none of the %d subschemas", len(lst))
		}
	}
	if lst, ok := sd["oneOf"].([]interface{}); ok { // an empty oneOf matches 0 subschemas → fails below
		matched := 0
		for _, sub := range lst {
			err := validateAgainstSchema(value, sub, root, depth+1)
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
	return nil
}

// resolveRef resolves an intra-document JSON-pointer $ref into root and returns the referenced
// subschema. Only intra-document pointers (`#` and `#/…`) are supported; an external URI is an
// error so an unresolvable ref FAILS validation rather than silently passing.
func resolveRef(ref string, root interface{}) (interface{}, error) {
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
		case map[string]interface{}:
			nv, found := c[tok]
			if !found {
				return nil, fmt.Errorf("$ref %q: %q not found", ref, tok)
			}
			cur = nv
		case []interface{}:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, fmt.Errorf("$ref %q: %q is not a valid array index", ref, tok)
			}
			cur = c[idx]
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
// additionalProperties schema. `true` (or a non-bool, non-object value) imposes nothing.
func checkAdditionalProps(d map[string]interface{}, properties, ap, root interface{}, depth int) error {
	declared := map[string]bool{}
	if props, ok := properties.(map[string]interface{}); ok {
		for k := range props {
			declared[k] = true
		}
	}
	allowed := true
	var apSchema interface{}
	switch a := ap.(type) {
	case bool:
		allowed = a
	case map[string]interface{}:
		apSchema = a
	}
	for k, cv := range d {
		if declared[k] {
			continue
		}
		if !allowed {
			return fmt.Errorf("additional property %q is not allowed", k)
		}
		if apSchema != nil {
			if err := validateAgainstSchema(cv, apSchema, root, depth+1); err != nil {
				return fmt.Errorf("additional property %q: %w", k, err)
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
