package main

import "testing"

func TestSplitRunArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		dash       int
		wantVendor string
		wantExtra  []string
		wantErr    bool
	}{
		{"vendor only", []string{"deepseek"}, -1, "deepseek", nil, false},
		{"no args (default)", nil, -1, "", nil, false},
		{"two positionals, no dash", []string{"a", "b"}, -1, "", nil, true},
		{"vendor + passthrough", []string{"deepseek", "--resume", "x"}, 1, "deepseek", []string{"--resume", "x"}, false},
		{"default + passthrough", []string{"--resume"}, 0, "", []string{"--resume"}, false},
		{"vendor + empty passthrough", []string{"deepseek"}, 1, "deepseek", nil, false},
		{"two positionals before dash", []string{"a", "b", "x"}, 2, "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vendor, extra, err := splitRunArgs(tc.args, tc.dash)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got vendor=%q extra=%v", vendor, extra)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if vendor != tc.wantVendor {
				t.Fatalf("vendor = %q, want %q", vendor, tc.wantVendor)
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
