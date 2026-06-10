package main

import "testing"

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name         string
		args         []string
		dash         int
		wantProvider string
		wantExtra    []string
		wantErr      bool
	}{
		{"provider only", []string{"deepseek"}, -1, "deepseek", nil, false},
		{"no args (default)", nil, -1, "", nil, false},
		{"two positionals, no dash", []string{"a", "b"}, -1, "", nil, true},
		{"provider + passthrough", []string{"deepseek", "--resume", "x"}, 1, "deepseek", []string{"--resume", "x"}, false},
		{"default + passthrough", []string{"--resume"}, 0, "", []string{"--resume"}, false},
		{"provider + empty passthrough", []string{"deepseek"}, 1, "deepseek", nil, false},
		{"two positionals before dash", []string{"a", "b", "x"}, 2, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider, extra, err := splitRunArgs(tc.args, tc.dash)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got provider=%q extra=%v", provider, extra)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if provider != tc.wantProvider {
				t.Fatalf("provider = %q, want %q", provider, tc.wantProvider)
			}
			if len(extra) != len(tc.wantExtra) {
				t.Fatalf("extra = %v, want %v", extra, tc.wantExtra)
			}
			for i := range extra {
				if extra[i] != tc.wantExtra[i] {
					t.Fatalf("extra = %v, want %v", extra, tc.wantExtra)
				}
			}
		})
	}
}
