package workflow

import (
	"encoding/json"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// allOf: a payload satisfying every subschema passes and flows back.
func TestSchemaAllOfValid(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"name":"x","employeeId":7}`)}
	})
	g, err := runScript(t, "co1", 1, leaf, `
res = agent("q", vendor="v", schema={"allOf": [
  {"type":"object","required":["name"],"properties":{"name":{"type":"string"}}},
  {"type":"object","required":["employeeId"],"properties":{"employeeId":{"type":"integer"}}}]})
n = res["name"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s, _ := starlark.AsString(g["n"]); s != "x" {
		t.Errorf("name = %q, want x", s)
	}
}

// allOf: violating any subschema fails terminally (one exec, no retry).
func TestSchemaAllOfViolationTerminal(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"name":"x"}`)} // missing employeeId
	})
	if _, err := runScript(t, "co2", 1, leaf, `
res = agent("q", vendor="v", schema={"allOf": [
  {"type":"object","required":["name"]},
  {"type":"object","required":["employeeId"]}]})
`); err == nil {
		t.Fatal("want a terminal validation error (allOf second subschema unmet)")
	}
	if n := len(rec.snapshot()); n != 1 {
		t.Errorf("execs = %d, want 1 (validation failure is terminal)", n)
	}
}

// oneOf: a payload matching more than one subschema fails (must match exactly one).
func TestSchemaOneOfMatchesTwoFails(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"card":"4111","bank":"acme"}`)}
	})
	if _, err := runScript(t, "co3", 1, leaf, `
res = agent("q", vendor="v", schema={"oneOf": [
  {"type":"object","required":["card"]},
  {"type":"object","required":["bank"]}]})
`); err == nil {
		t.Fatal("want an error: payload matches 2 oneOf subschemas, not exactly 1")
	}
}

// $ref: an intra-document pointer into $defs is resolved and enforced; a conforming payload passes.
func TestSchemaRefResolved(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"shipTo":{"zip":"10001"}}`)}
	})
	g, err := runScript(t, "co4", 1, leaf, `
res = agent("q", vendor="v", schema={
  "type":"object","required":["shipTo"],
  "properties":{"shipTo":{"$ref":"#/$defs/addr"}},
  "$defs":{"addr":{"type":"object","required":["zip"],"properties":{"zip":{"type":"string"}}}}})
z = res["shipTo"]["zip"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s, _ := starlark.AsString(g["z"]); s != "10001" {
		t.Errorf("zip = %q, want 10001", s)
	}
}

// oneOf surfaces a dangling-$ref structural error in a sibling branch even when another branch matches.
func TestSchemaOneOfSurfacesBrokenRef(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"x"`)}
	})
	if _, err := runScript(t, "co6", 1, leaf, `
res = agent("q", vendor="v", schema={"oneOf": [
  {"$ref":"#/missing"},
  {"type":"string"}]})
`); err == nil {
		t.Fatal("want an error: oneOf has an unresolvable $ref branch (must surface, not swallow)")
	}
}

// anyOf scans every branch, so a structural error is not hidden by an earlier match.
func TestSchemaAnyOfSurfacesBrokenRefAfterMatch(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"x"`)}
	})
	if _, err := runScript(t, "co8", 1, leaf, `
res = agent("q", vendor="v", schema={"anyOf": [
  {"type":"string"},
  {"$ref":"#/missing"}]})
`); err == nil {
		t.Fatal("want an error: anyOf has a broken $ref branch after a matching branch")
	}
}

// A structural defect surfaces even behind a sibling keyword that would short-circuit: the $ref is
// checked before the mismatching type, so the value can't slip through.
func TestSchemaAnyOfDefectBehindMismatch(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"x"`)}
	})
	if _, err := runScript(t, "co9", 1, leaf, `
res = agent("q", vendor="v", schema={"anyOf": [
  {"type":"string"},
  {"type":"number","$ref":"#/missing"}]})
`); err == nil {
		t.Fatal("want an error: the broken $ref behind a type mismatch must still surface")
	}
}

// allOf scans every branch and surfaces a structural defect (broken $ref) even when an earlier
// branch is a plain value mismatch — so the defect isn't hidden inside an outer anyOf.
func TestSchemaAllOfDefectBehindMismatch(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"x"`)}
	})
	if _, err := runScript(t, "co10", 1, leaf, `
res = agent("q", vendor="v", schema={"anyOf": [
  {"type":"string"},
  {"allOf":[{"type":"number"}, {"$ref":"#/missing"}]}]})
`); err == nil {
		t.Fatal("want an error: the broken $ref in allOf[1] must surface past the allOf[0] mismatch")
	}
}

// An empty anyOf matches nothing and an empty oneOf matches zero subschemas — both fail, not accept.
func TestSchemaEmptyAnyOfOneOfFail(t *testing.T) {
	for i, schema := range []string{`{"anyOf":[]}`, `{"oneOf":[]}`} {
		l := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
			return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"x"`)}
		})
		if _, err := runScript(t, "coE"+string(rune('a'+i)), 1, l,
			"res = agent(\"q\", vendor=\"v\", schema="+schema+")"); err == nil {
			t.Fatalf("want an error: %s must match nothing", schema)
		}
	}
}

// $ref resolves an array index (RFC6901 JSON pointer): #/$defs/opts/1 picks the 2nd schema.
func TestSchemaRefArrayIndex(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"x":"hi"}`)}
	})
	g, err := runScript(t, "co7", 1, leaf, `
res = agent("q", vendor="v", schema={
  "type":"object","properties":{"x":{"$ref":"#/$defs/opts/1"}},
  "$defs":{"opts":[{"type":"integer"},{"type":"string"}]}})
x = res["x"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s, _ := starlark.AsString(g["x"]); s != "hi" {
		t.Errorf("x = %q, want hi", s)
	}
}

// $ref: a violation of the referenced subschema fails (zip must be a string).
func TestSchemaRefViolation(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"shipTo":{"zip":123}}`)}
	})
	if _, err := runScript(t, "co5", 1, leaf, `
res = agent("q", vendor="v", schema={
  "type":"object","properties":{"shipTo":{"$ref":"#/$defs/addr"}},
  "$defs":{"addr":{"type":"object","required":["zip"],"properties":{"zip":{"type":"string"}}}}})
`); err == nil {
		t.Fatal("want an error: $ref target requires zip:string, got integer")
	}
}

// An external (non intra-document) $ref is unsupported by the local backstop and fails validation
// (claude's --json-schema is authoritative for it).
func TestSchemaExternalRefFails(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a":1}`)}
	})
	if _, err := runScript(t, "co11", 1, leaf,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"a":{"$ref":"https://example.com/s.json"}}})`); err == nil {
		t.Fatal("want an error: an external $ref is unsupported by the local backstop")
	}
}

// pattern: a string must match the regex; a mismatch fails.
func TestSchemaPattern(t *testing.T) {
	ok := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"code":"AB12"}`)}
	})
	if _, err := runScript(t, "pt1", 1, ok,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"code":{"type":"string","pattern":"^[A-Z]{2}[0-9]{2}$"}}})`); err != nil {
		t.Fatalf("matching pattern should pass: %v", err)
	}
	bad := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"code":"xx"}`)}
	})
	if _, err := runScript(t, "pt2", 1, bad,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"code":{"type":"string","pattern":"^[A-Z]{2}[0-9]{2}$"}}})`); err == nil {
		t.Fatal("want a pattern-mismatch error")
	}
}

// An ECMA-only pattern (lookahead) Go RE2 can't compile is skipped locally, so a conforming value passes.
func TestSchemaPatternEcmaSkipped(t *testing.T) {
	leaf := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"p":"x1"}`)}
	})
	if _, err := runScript(t, "pt3", 1, leaf,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"p":{"type":"string","pattern":"(?=.*[0-9])"}}})`); err != nil {
		t.Fatalf("an uncompilable (ECMA) pattern should be skipped, not fail: %v", err)
	}
}

// format: a known format (email) is enforced; an invalid value fails.
func TestSchemaFormat(t *testing.T) {
	good := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"e":"a@b.com"}`)}
	})
	if _, err := runScript(t, "fm1", 1, good,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"e":{"type":"string","format":"email"}}})`); err != nil {
		t.Fatalf("valid email should pass: %v", err)
	}
	bad := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"e":"nope"}`)}
	})
	if _, err := runScript(t, "fm2", 1, bad,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"e":{"type":"string","format":"email"}}})`); err == nil {
		t.Fatal("want an invalid-email error")
	}
}

// additionalProperties:false rejects a key not named in properties; a clean object passes.
func TestSchemaAdditionalPropertiesFalse(t *testing.T) {
	extra := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a":1,"b":2}`)}
	})
	if _, err := runScript(t, "ap1", 1, extra,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"a":{"type":"integer"}},"additionalProperties":False})`); err == nil {
		t.Fatal("want an additional-property error (b not allowed)")
	}
	clean := fakeLeaf(&recorder{}, func(leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"a":1}`)}
	})
	if _, err := runScript(t, "ap2", 1, clean,
		`res = agent("q", vendor="v", schema={"type":"object","properties":{"a":{"type":"integer"}},"additionalProperties":False})`); err != nil {
		t.Fatalf("a clean object should pass: %v", err)
	}
}
