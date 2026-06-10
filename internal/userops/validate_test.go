package userops

import (
	"strings"
	"testing"
)

func TestValidateProviderName_OK(t *testing.T) {
	cases := []string{
		"a",
		"glm",
		"deepseek-v4",
		"provider_1",
		"Acc3pt",
		// boundary: 32 chars (1 letter + 31 follow chars)
		"a" + strings.Repeat("x", 31),
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateProviderName(name); err != nil {
				t.Fatalf("ValidateProviderName(%q) = %v; want nil", name, err)
			}
		})
	}
}

func TestValidateProviderName_Reject(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"leading-digit", "1foo"},
		{"leading-hyphen", "-foo"},
		{"leading-underscore", "_foo"},
		{"dot", "foo.bar"},
		{"slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"dotdot", ".."},
		{"path-traversal", "../etc"},
		{"space", "foo bar"},
		{"too-long", "a" + strings.Repeat("x", 32)}, // 33 chars total
		{"unicode", "日本"},
		{"null", "a\x00b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProviderName(tc.in)
			if err == nil {
				t.Fatalf("ValidateProviderName(%q) = nil; want error", tc.in)
			}
		})
	}
}
