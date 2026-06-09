package config

import "testing"

func TestResolveModel(t *testing.T) {
	v := &Vendor{DefaultModel: "d", StrongModel: "s", FastModel: "f"}
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
	blank := &Vendor{DefaultModel: "d"}
	if got := blank.ResolveModel("strong"); got != "d" {
		t.Errorf("blank strong slot: ResolveModel(strong) = %q, want d", got)
	}
	if got := blank.ResolveModel("fast"); got != "d" {
		t.Errorf("blank fast slot: ResolveModel(fast) = %q, want d", got)
	}
}

func TestStrongFastModelOrDefault(t *testing.T) {
	set := &Vendor{DefaultModel: "d", StrongModel: "s", FastModel: "f"}
	if set.StrongModelOrDefault() != "s" || set.FastModelOrDefault() != "f" {
		t.Errorf("set slots: strong=%q fast=%q", set.StrongModelOrDefault(), set.FastModelOrDefault())
	}
	blank := &Vendor{DefaultModel: "d"}
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
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if len(got) != len(want) {
		t.Fatalf("EffortLevels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EffortLevels()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestValidate_EffortAndPermission verifies the config-strict gate rejects an
// invalid effort or default_permission and accepts the empty (unset) value.
func TestValidate_EffortAndPermission(t *testing.T) {
	base := func() *Vendor {
		return &Vendor{
			Name: "v", BaseURL: "https://x/anthropic", DefaultModel: "d",
			ModelsEndpoint: "https://x/v1/models", SecretBackend: "file", SecretRef: "v.key",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Vendor)
		wantErr bool
	}{
		{"effort empty ok", func(v *Vendor) { v.Effort = "" }, false},
		{"effort max ok", func(v *Vendor) { v.Effort = "max" }, false},
		{"effort bad", func(v *Vendor) { v.Effort = "ultra" }, true},
		{"perm empty ok", func(v *Vendor) { v.DefaultPermission = "" }, false},
		{"perm acceptEdits ok", func(v *Vendor) { v.DefaultPermission = "acceptEdits" }, false},
		{"perm bad", func(v *Vendor) { v.DefaultPermission = "yolo" }, true},
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
