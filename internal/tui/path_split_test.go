package tui

import "testing"

// TestProjectNameLabel covers both rail forms across forward-slash, Windows
// backslash, bare-name, and root inputs — a backslash cwd must split into real
// segments, not render as one long absolute path.
func TestProjectNameLabel(t *testing.T) {
	cases := []struct {
		dir       string
		wantName  string
		wantLabel string
	}{
		{"", "(no project)", "(no project)"},
		{"/a/b", "b", "a/b"},
		{"/a/b/c", "c", "b/c"},
		{`C:\Users\me\proj\sub`, "sub", "proj/sub"},
		{`C:\proj`, "proj", "C:/proj"},
		{"proj", "proj", "proj"},
		{"/single", "single", "single"},
		{"/", "", ""},
		{`\`, "", ""},
	}
	for _, c := range cases {
		if got := projectName(c.dir); got != c.wantName {
			t.Errorf("projectName(%q) = %q, want %q", c.dir, got, c.wantName)
		}
		if got := projectLabel(c.dir); got != c.wantLabel {
			t.Errorf("projectLabel(%q) = %q, want %q", c.dir, got, c.wantLabel)
		}
	}
}
