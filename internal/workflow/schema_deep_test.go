package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestDeepSchemaNestedValid: a payload satisfying a nested object/array/integer schema
// passes and the parsed value flows back.
func TestDeepSchemaNestedValid(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"user":{"id":7,"tags":["a","b"]}}`)}
	})
	v, err := runScript(t, "ds1", 1, leaf, `
const res = await agent("q", {provider: "v", schema: {
  type: "object", required: ["user"],
  properties: {user: {type: "object", required: ["id", "tags"],
    properties: {id: {type: "integer"}, tags: {type: "array", items: {type: "string"}}}}}}});
return { uid: res.user.id };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := intField(t, wantMap(t, v), "uid"); n != 7 {
		t.Errorf("uid = %v, want 7", n)
	}
}

// TestDeepSchemaNestedTypeMismatchTerminal: a nested type violation (id should be an
// integer) fails validation TERMINALLY after exactly one exec — the deep validator
// catches what a shallow key-presence check would have passed.
func TestDeepSchemaNestedTypeMismatchTerminal(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"user":{"id":"not-an-int"}}`)}
	})
	_, err := runScript(t, "ds2", 1, leaf, `
return await agent("q", {provider: "v", schema: {type: "object",
  properties: {user: {type: "object", properties: {id: {type: "integer"}}}}}});
`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("expected a deep-schema failure, got %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("attempts = %d, want exactly 1 (validation failure is terminal)", n)
	}
}

// TestDeepSchemaIntegerAcceptsZeroFractionFloat: a provider emitting 5.0 for an integer
// field is accepted (zero-fraction float == integer).
func TestDeepSchemaIntegerAcceptsZeroFractionFloat(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"n":5.0}`)}
	})
	if _, err := runScript(t, "ds3", 1, leaf,
		`return await agent("q", {provider: "v", schema: {properties: {n: {type: "integer"}}}});`); err != nil {
		t.Fatalf("5.0 must satisfy an integer field: %v", err)
	}
}

// TestDeepSchemaRequiredRejectsScalar: a schema with `required` (or `properties`) must
// REJECT a non-object payload (e.g. the bare string "oops") rather than letting it slip
// through — the object constraints imply the value is an object.
func TestDeepSchemaRequiredRejectsScalar(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`"oops"`)} // valid JSON, but a scalar string
	})
	_, err := runScript(t, "dssc", 1, leaf,
		`return await agent("q", {provider: "v", schema: {required: ["answer"]}});`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("a scalar payload must fail a required-keys schema, got %v", err)
	}
}

// TestDeepSchemaEnum: a value outside the enum is a terminal failure (one exec); an
// allowed value passes.
func TestDeepSchemaEnum(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"color":"purple"}`)} // not in enum
	})
	_, err := runScript(t, "ds4", 1, leaf,
		`return await agent("q", {provider: "v", schema: {properties: {color: {enum: ["red", "green", "blue"]}}}});`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("an out-of-enum value must fail, got %v", err)
	}
	if n := len(rec.prompts()); n != 1 {
		t.Errorf("attempts = %d, want exactly 1 (enum violation is terminal)", n)
	}

	rec2 := &recorder{}
	leaf2 := fakeLeaf(rec2, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, StructuredOutput: json.RawMessage(`{"color":"red"}`)}
	})
	v, err := runScript(t, "ds4b", 1, leaf2, `
const res = await agent("q", {provider: "v", schema: {properties: {color: {enum: ["red", "green", "blue"]}}}});
return { c: res.color };
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if s := strField(t, wantMap(t, v), "c"); s != "red" {
		t.Errorf("c = %q, want red", s)
	}
}
