package tui

import (
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// TestDefaultProvider_CursorReanchorsAfterHoist: when a refresh marks a different
// provider the default (hoisting it to the top), the cursor stays on the SAME
// provider it was on, not whatever now occupies that index.
func TestDefaultProvider_CursorReanchorsAfterHoist(t *testing.T) {
	m := withProviders(t,
		userops.ProviderView{Name: "aaa", Enabled: true},
		userops.ProviderView{Name: "bbb", Enabled: true},
		userops.ProviderView{Name: "ccc", Enabled: true},
	)
	m.providerCursor = 1
	if got := m.providers[m.providerCursor].Name; got != "bbb" {
		t.Fatalf("setup: cursor on %q, want bbb", got)
	}
	// A refresh now marks ccc the default → ccc hoists to index 0.
	m, _ = step(t, m, providersMsg{
		defaultProvider: "ccc",
		providers: []userops.ProviderView{
			{Name: "aaa", Enabled: true},
			{Name: "bbb", Enabled: true},
			{Name: "ccc", Enabled: true, Default: true},
		},
	})
	if m.providers[0].Name != "ccc" {
		t.Fatalf("default ccc not hoisted to top, got %q", m.providers[0].Name)
	}
	if got := m.providers[m.providerCursor].Name; got != "bbb" {
		t.Fatalf("cursor jumped to %q after re-sort, want bbb", got)
	}
}

// TestDefaultProvider_DisabledRefused: pressing the default key on a DISABLED
// provider refuses with an info modal and dispatches nothing.
func TestDefaultProvider_DisabledRefused(t *testing.T) {
	m := withProviders(t,
		userops.ProviderView{Name: "live", Enabled: true},
		userops.ProviderView{Name: "off", Enabled: false},
	)
	m.providerCursor = 1
	if got := m.providers[m.providerCursor].Name; got != "off" {
		t.Fatalf("setup: cursor on %q, want off", got)
	}
	m2, cmd := press(t, m, "s")
	if cmd != nil {
		t.Fatalf("setting a disabled default must not dispatch a command")
	}
	if m2.confirm == nil || !strings.Contains(m2.confirm.result, "disabled") {
		t.Fatalf("want an info modal naming 'disabled', got confirm=%+v", m2.confirm)
	}
}

// TestDefaultProvider_UnsetDisabledAllowed: clearing a default that is itself
// disabled is still allowed (the unset case precedes the disabled gate).
func TestDefaultProvider_UnsetDisabledAllowed(t *testing.T) {
	m := withProviders(t,
		userops.ProviderView{Name: "live", Enabled: true},
		userops.ProviderView{Name: "off", Enabled: false},
	)
	m.defaultProvider = "off" // pinned but disabled
	// cursor onto "off" (disabled rows sort after enabled within the class).
	for i, v := range m.providers {
		if v.Name == "off" {
			m.providerCursor = i
		}
	}
	m2, _ := press(t, m, "s")
	if m2.confirm == nil || m2.confirm.kind != confirmUnsetDefault {
		t.Fatalf("want an unset-default confirm, got %+v", m2.confirm)
	}
}
