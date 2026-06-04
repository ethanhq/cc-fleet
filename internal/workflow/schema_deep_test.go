package workflow

import (
	"strings"
	"sync"
	"testing"

	"go.starlark.net/starlark"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// TestDeepSchemaNestedValid: a reply satisfying a nested object/array/integer schema
// passes and the parsed value flows back.
func TestDeepSchemaNestedValid(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `{"user":{"id":7,"tags":["a","b"]}}`}
	})
	g, err := runScript(t, "ds1", 1, leaf, `
res = agent("q", vendor="v", schema={
  "type": "object", "required": ["user"],
  "properties": {"user": {"type": "object", "required": ["id", "tags"],
    "properties": {"id": {"type": "integer"}, "tags": {"type": "array", "items": {"type": "string"}}}}}})
uid = res["user"]["id"]
`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if i, _ := starlark.AsInt32(g["uid"]); i != 7 {
		t.Errorf("uid = %v, want 7", g["uid"])
	}
}

// TestDeepSchemaNestedTypeMismatchRetriesThenFails: a nested type violation (id should be
// an integer) fails validation, retries, and ultimately aborts — the deep validator
// catches what the v1 shallow key-presence check would have passed.
func TestDeepSchemaNestedTypeMismatchRetriesThenFails(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `{"user":{"id":"not-an-int"}}`}
	})
	_, err := runScript(t, "ds2", 1, leaf, `
res = agent("q", vendor="v", schema={"type": "object",
  "properties": {"user": {"type": "object", "properties": {"id": {"type": "integer"}}}}})
`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("expected a deep-schema failure, got %v", err)
	}
	if n := len(rec.prompts()); n != 3 {
		t.Errorf("attempts = %d, want 3 (retried on the nested type mismatch)", n)
	}
}

// TestDeepSchemaIntegerAcceptsZeroFractionFloat: a vendor emitting 5.0 for an integer
// field is accepted (zero-fraction float == integer).
func TestDeepSchemaIntegerAcceptsZeroFractionFloat(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `{"n":5.0}`}
	})
	if _, err := runScript(t, "ds3", 1, leaf,
		`res = agent("q", vendor="v", schema={"properties": {"n": {"type": "integer"}}})`); err != nil {
		t.Fatalf("5.0 must satisfy an integer field: %v", err)
	}
}

// TestDeepSchemaRequiredRejectsScalar: a schema with `required` (or `properties`) must
// REJECT a non-object reply (e.g. the bare string "oops") rather than letting it slip
// through — the object constraints imply the value is an object.
func TestDeepSchemaRequiredRejectsScalar(t *testing.T) {
	rec := &recorder{}
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		return subagent.Result{OK: true, Result: `"oops"`} // valid JSON, but a scalar string
	})
	_, err := runScript(t, "dssc", 1, leaf,
		`res = agent("q", vendor="v", schema={"required": ["answer"]})`)
	if err == nil || !strings.Contains(err.Error(), "schema not satisfied") {
		t.Fatalf("a scalar reply must fail a required-keys schema, got %v", err)
	}
}

// TestDeepSchemaEnum: a value outside the enum fails (and retries); an allowed value passes.
func TestDeepSchemaEnum(t *testing.T) {
	rec := &recorder{}
	var mu sync.Mutex
	n := 0
	leaf := fakeLeaf(rec, func(c leafCall) subagent.Result {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			return subagent.Result{OK: true, Result: `{"color":"purple"}`} // not in enum
		}
		return subagent.Result{OK: true, Result: `{"color":"red"}`}
	})
	g, err := runScript(t, "ds4", 1, leaf,
		`res = agent("q", vendor="v", schema={"properties": {"color": {"enum": ["red", "green", "blue"]}}})
c = res["color"]`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if asStr(t, g["c"]) != "red" {
		t.Errorf("c = %v, want red (enum retry recovered)", g["c"])
	}
	if n < 2 {
		t.Errorf("expected an enum retry, leaf ran %d times", n)
	}
}
