package workflow

import (
	"context"
	"strings"
	"testing"
)

// runWithDefault mirrors runScript but seeds the engine's recorded default-provider
// resolution (set at mint in production), so a vendor-less agent() resolves against it.
func runWithDefault(t *testing.T, provider, errCode, src string) (*recorder, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	oldR := resolveProfile
	resolveProfile = func(requested string) (string, string) { return requested, "" }
	t.Cleanup(func() { resolveProfile = oldR })
	eng := newTestEngine(context.Background(), "rdp", 1)
	eng.defaultProvider, eng.defaultProviderErr = provider, errCode
	_, err := eng.run("test.js", []byte(src), Options{})
	return rec, err
}

// TestAgentVendorlessUsesDefault: a vendor-less agent() runs on the recorded default
// provider (the leaf request carries it, so the journal key folds it like a named one).
func TestAgentVendorlessUsesDefault(t *testing.T) {
	rec, err := runWithDefault(t, "glm", "", `return await agent("p", {});`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].vendor != "glm" {
		t.Fatalf("leaf vendor = %q, want glm (the recorded default)", calls[0].vendor)
	}
}

// TestAgentExplicitVendorBeatsDefault: opts.vendor always wins over the default.
func TestAgentExplicitVendorBeatsDefault(t *testing.T) {
	rec, err := runWithDefault(t, "glm", "", `return await agent("p", {vendor: "kimi"});`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].vendor != "kimi" {
		t.Fatalf("leaf vendor = %v, want kimi", calls)
	}
}

// TestAgentVendorlessNoDefaultThrows: with no recorded provider, a vendor-less agent()
// throws the recorded error code (NO_DEFAULT_PROVIDER), failing the run.
func TestAgentVendorlessNoDefaultThrows(t *testing.T) {
	rec, err := runWithDefault(t, "", "NO_DEFAULT_PROVIDER", `return await agent("p", {});`)
	if err == nil {
		t.Fatal("run: want a failure for a vendor-less agent with no default, got nil")
	}
	if !strings.Contains(err.Error(), "NO_DEFAULT_PROVIDER") {
		t.Fatalf("err = %v, want it to name NO_DEFAULT_PROVIDER", err)
	}
	if len(rec.snapshot()) != 0 {
		t.Fatalf("calls = %d, want 0 (no leaf should run)", len(rec.snapshot()))
	}
}
