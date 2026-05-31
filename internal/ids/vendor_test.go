package ids

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateVendorName_Accept keeps the vendor-name grammar practical for
// real vendor ids.
func TestValidateVendorName_Accept(t *testing.T) {
	cases := []string{
		"a",
		"glm",
		"deepseek-v4",
		"vendor_1",
		"Acc3pt",
		"a" + strings.Repeat("x", 31), // exactly 32 chars
	}
	for _, name := range cases {
		if err := ValidateVendorName(name); err != nil {
			t.Errorf("ValidateVendorName(%q) = %v; want nil", name, err)
		}
	}
}

// TestValidateVendorName_Reject: a hand-edited vendors.toml table name that's
// path-traversal ("../escape") or shell-injection ("bad;touch x", "$(...)")
// shaped must fail the grammar, since the name flows into a filepath.Join
// (profile path) AND a shell-evaluated apiKeyHelper.
func TestValidateVendorName_Reject(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"leading-digit", "1foo"},
		{"leading-hyphen", "-foo"},
		{"leading-underscore", "_foo"},
		{"dot", "foo.bar"},
		{"path-traversal", "../escape"},
		{"slash", "foo/bar"},
		{"backslash", `foo\bar`},
		{"dotdot", ".."},
		{"shell-semicolon", "bad;touch x"},
		{"shell-subshell", "x$(rm -rf /)"},
		{"shell-backtick", "x`id`"},
		{"space", "foo bar"},
		{"too-long", "a" + strings.Repeat("x", 32)}, // 33 chars
		{"unicode", "日本"},
		{"null", "a\x00b"},
		{"leading-percent", "%x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVendorName(tc.in)
			if err == nil {
				t.Fatalf("ValidateVendorName(%q) = nil; want error", tc.in)
			}
			if !errors.Is(err, ErrInvalidVendorName) {
				t.Fatalf("ValidateVendorName(%q): err=%v, want ErrInvalidVendorName", tc.in, err)
			}
		})
	}
}
