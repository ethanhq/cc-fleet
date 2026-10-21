package config

import (
	"slices"
	"testing"
)

func TestResolveModel(t *testing.T) {
	v := &Provider{DefaultModel: "d", StrongModel: "s", FastModel: "f"}
	cases := map[string]string{
		"":            "d", // empty → default
		"default":     "d",
		"strong":      "s",
		"fast":        "f",
		"literal-foo": "literal-foo", // any other value is a literal id
	}
	for in, want := range cases {
		if got := v.ResolveModel(in); got != want {
			t.Errorf("ResolveModel(%q) = %q, want %q", in, got, want)
		}
	}

	// Blank strong/fast slots fall back to the default via the keywords.
	blank := &Provider{DefaultModel: "d"}
	if got := blank.ResolveModel("strong"); got != "d" {
		t.Errorf("blank strong slot: ResolveModel(strong) = %q, want d", got)
	}
	if got := blank.ResolveModel("fast"); got != "d" {
		t.Errorf("blank fast slot: ResolveModel(fast) = %q, want d", got)
	}
}

func TestResolveEffort(t *testing.T) {
	v := &Provider{Effort: "high", StrongEffort: "max", FastEffort: "low"}
	cases := map[string]string{
		"":                "high",
		"default":         "high",
		"strong":          "max",
		"fast":            "low",
		"strong-model-id": "high", // a literal id runs at the default slot's effort
	}
	for in, want := range cases {
		if got := v.ResolveEffort(in); got != want {
			t.Errorf("ResolveEffort(%q) = %q, want %q", in, got, want)
		}
	}

	// Blank strong/fast efforts follow Effort, including an unset one.
	for _, p := range []*Provider{{Effort: "medium"}, {}} {
		for _, slot := range []string{"strong", "fast"} {
			if got := p.ResolveEffort(slot); got != p.Effort {
				t.Errorf("blank %s effort with Effort %q: got %q", slot, p.Effort, got)
			}
		}
	}
}

func TestStrongFastModelOrDefault(t *testing.T) {
	set := &Provider{DefaultModel: "d", StrongModel: "s", FastModel: "f"}
	if set.StrongModelOrDefault() != "s" || set.FastModelOrDefault() != "f" {
		t.Errorf("set slots: strong=%q fast=%q", set.StrongModelOrDefault(), set.FastModelOrDefault())
	}
	blank := &Provider{DefaultModel: "d"}
	if blank.StrongModelOrDefault() != "d" || blank.FastModelOrDefault() != "d" {
		t.Errorf("blank slots should follow default: strong=%q fast=%q",
			blank.StrongModelOrDefault(), blank.FastModelOrDefault())
	}
}

func TestContextMarker1M(t *testing.T) {
	if got := With1M("m"); got != "m[1m]" {
		t.Errorf("With1M(m) = %q, want m[1m]", got)
	}
	if got := With1M("m[1m]"); got != "m[1m]" { // idempotent
		t.Errorf("With1M is not idempotent: %q", got)
	}
	if got := With1M(""); got != "" {
		t.Errorf("With1M(\"\") = %q, want \"\"", got)
	}
	if got := Strip1M("m[1m]"); got != "m" {
		t.Errorf("Strip1M(m[1m]) = %q, want m", got)
	}
	if got := Strip1M("m"); got != "m" { // no-op when absent
		t.Errorf("Strip1M(m) = %q, want m", got)
	}
	if got := Strip1M("a[1m]b"); got != "a[1m]b" { // only a TRAILING marker is stripped
		t.Errorf("Strip1M(a[1m]b) = %q, want a[1m]b", got)
	}
	if !Has1M("m[1m]") || Has1M("m") {
		t.Errorf("Has1M mismatch: m[1m]=%v m=%v", Has1M("m[1m]"), Has1M("m"))
	}
}

func TestEffortLevels(t *testing.T) {
	got := EffortLevels()
	want := []string{"off", "low", "medium", "high", "xhigh", "max"}
	if len(got) != len(want) {
		t.Fatalf("EffortLevels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EffortLevels()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// An unset effort passes nothing, off turns thinking off instead of naming a level.
func TestEffortArgs(t *testing.T) {
	for in, want := range map[string][]string{
		"":     nil,
		"off":  {"--thinking", "disabled"},
		"high": {"--effort", "high"},
		"max":  {"--effort", "max"},
	} {
		if got := EffortArgs(in); !slices.Equal(got, want) {
			t.Errorf("EffortArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestValidate_EffortAndPermission verifies the config-strict gate rejects an
// invalid effort or default_permission and accepts the empty (unset) value.
func TestValidate_EffortAndPermission(t *testing.T) {
	base := func() *Provider {
		return &Provider{
			Name: "v", BaseURL: "https://x/anthropic", DefaultModel: "d",
			ModelsEndpoint: "https://x/v1/models", SecretBackend: "file", SecretRef: "v.key",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Provider)
		wantErr bool
	}{
		{"effort empty ok", func(v *Provider) { v.Effort = "" }, false},
		{"effort max ok", func(v *Provider) { v.Effort = "max" }, false},
		{"effort off ok", func(v *Provider) { v.Effort = "off" }, false},
		{"effort bad", func(v *Provider) { v.Effort = "ultra" }, true},
		{"slot efforts ok", func(v *Provider) { v.StrongEffort, v.FastEffort = "max", "low" }, false},
		{"strong effort bad", func(v *Provider) { v.StrongEffort = "ultra" }, true},
		{"fast effort bad", func(v *Provider) { v.FastEffort = "HIGH" }, true},
		{"perm empty ok", func(v *Provider) { v.DefaultPermission = "" }, false},
		{"perm acceptEdits ok", func(v *Provider) { v.DefaultPermission = "acceptEdits" }, false},
		{"perm bad", func(v *Provider) { v.DefaultPermission = "yolo" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.mutate(v)
			err := v.validate("v")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
