package teammate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInboxPathSanitizes(t *testing.T) {
	cases := []struct{ in, dir, inbox string }{
		{"session-7c8f769b", "session-7c8f769b", "session-7c8f769b"},
		{"My_Team.v2", "my-team-v2", "My_Team-v2"},
		{"a@b c", "a-b-c", "a-b-c"},
		{"团队", "--", "--"},
		{"x\U0001F600y", "x--y", "x--y"}, // outside the BMP: two UTF-16 code units
		{"../..", "-----", "-----"},
	}
	for _, tc := range cases {
		if got := SanitizeTeamDir(tc.in); got != tc.dir {
			t.Errorf("SanitizeTeamDir(%q) = %q, want %q", tc.in, got, tc.dir)
		}
		if got := SanitizeInboxName(tc.in); got != tc.inbox {
			t.Errorf("SanitizeInboxName(%q) = %q, want %q", tc.in, got, tc.inbox)
		}
	}

	h := hermeticHome(t)
	got, err := InboxPath("My_Team", "Worker_1@x")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.ClaudeDir, "teams", "My_Team", "inboxes", "Worker_1-x.json")
	if got != want {
		t.Fatalf("InboxPath = %q, want %q", got, want)
	}
}

func TestInboxPathStaysUnderRoot(t *testing.T) {
	h := hermeticHome(t)
	root := filepath.Join(h.ClaudeDir, "teams") + string(os.PathSeparator)
	for _, tc := range [][2]string{
		{"..", ".."},
		{"../../etc", "passwd"},
		{"t", "../../../x"},
		{"/abs", "/abs"},
		{`..\..`, `..\x`},
		{".", "."},
	} {
		got, err := InboxPath(tc[0], tc[1])
		if err != nil {
			t.Errorf("InboxPath(%q, %q): %v", tc[0], tc[1], err)
			continue
		}
		if !strings.HasPrefix(got, root) || strings.Count(strings.TrimPrefix(got, root), string(os.PathSeparator)) != 2 {
			t.Errorf("InboxPath(%q, %q) = %q escapes or reshapes %s<team>/inboxes/<name>.json", tc[0], tc[1], got, root)
		}
	}
	for _, tc := range [][2]string{{"", "x"}, {"t", ""}} {
		if got, err := InboxPath(tc[0], tc[1]); err == nil {
			t.Errorf("InboxPath(%q, %q) = %q, want an error", tc[0], tc[1], got)
		}
	}
}
