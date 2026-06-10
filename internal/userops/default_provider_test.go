package userops

import (
	"errors"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// TestSetDefaultProvider_RefusesOverwriteWithoutForce: the guard the skill relies
// on to only ever FILL a blank default — a second set without --force fails.
func TestSetDefaultProvider_RefusesOverwriteWithoutForce(t *testing.T) {
	setupHome(t)
	seedVendor(t, "glm")
	seedVendor(t, "kimi")

	if _, err := SetDefaultProvider("glm", false); err != nil {
		t.Fatalf("first set: %v", err)
	}
	_, err := SetDefaultProvider("kimi", false)
	if err == nil {
		t.Fatal("second set without force: want DEFAULT_ALREADY_SET, got nil")
	}
	var op *Op
	if !errors.As(err, &op) || op.Code != CodeDefaultAlreadySet {
		t.Fatalf("err = %v, want DEFAULT_ALREADY_SET", err)
	}
	// force overwrites.
	view, err := SetDefaultProvider("kimi", true)
	if err != nil {
		t.Fatalf("forced set: %v", err)
	}
	if view.Provider != "kimi" || view.Source != "configured" {
		t.Fatalf("view = %+v, want kimi/configured", view)
	}
}

// TestSetDefaultProvider_UnknownRejected: pinning a provider that isn't configured
// fails (the value must exist; disabled is allowed).
func TestSetDefaultProvider_UnknownRejected(t *testing.T) {
	setupHome(t)
	seedVendor(t, "glm")
	_, err := SetDefaultProvider("nope", false)
	var op *Op
	if !errors.As(err, &op) || op.Code != CodeVendorUnknown {
		t.Fatalf("err = %v, want VENDOR_UNKNOWN", err)
	}
}

// TestUnsetDefaultProvider clears the pin; show then reports the sole-enabled auto.
func TestUnsetDefaultProvider(t *testing.T) {
	setupHome(t)
	seedVendor(t, "glm")
	if _, err := SetDefaultProvider("glm", false); err != nil {
		t.Fatalf("set: %v", err)
	}
	view, err := UnsetDefaultProvider()
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	// Only one provider remains, so it serves as the auto default.
	if view.Provider != "glm" || view.Source != "auto" || view.Configured != "" {
		t.Fatalf("view = %+v, want glm/auto/'' ", view)
	}
}

// TestRemove_ScrubsDefault: removing the default provider clears default_provider
// so the on-disk config never carries a dangling pointer.
func TestRemove_ScrubsDefault(t *testing.T) {
	setupHome(t)
	seedVendor(t, "glm")
	seedVendor(t, "kimi")
	if _, err := SetDefaultProvider("glm", false); err != nil {
		t.Fatalf("set: %v", err)
	}
	res, err := Remove(RemoveRequest{Name: "glm"})
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !res.DefaultCleared {
		t.Fatal("RemoveResult.DefaultCleared = false, want true")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.DefaultProvider != "" {
		t.Fatalf("default_provider = %q after removing the default, want empty", cfg.DefaultProvider)
	}
}

// TestList_ExposesDefault: List surfaces the configured default top-level + the
// per-row Default flag on the effective default.
func TestList_ExposesDefault(t *testing.T) {
	setupHome(t)
	seedVendor(t, "glm")
	seedVendor(t, "kimi")
	if _, err := SetDefaultProvider("kimi", false); err != nil {
		t.Fatalf("set: %v", err)
	}
	res, err := List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if res.DefaultProvider != "kimi" {
		t.Fatalf("ListResult.DefaultProvider = %q, want kimi", res.DefaultProvider)
	}
	for _, v := range res.Vendors {
		want := v.Name == "kimi"
		if v.Default != want {
			t.Fatalf("row %q Default = %v, want %v", v.Name, v.Default, want)
		}
	}
}
