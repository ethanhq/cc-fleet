package sessiontitle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// TestCleanTitle_StripsControlBytes: CleanTitle drops non-whitespace control
// runes (ANSI ESC 0x1b / BEL / OSC sequences a /rename title could carry into
// the TUI board header — injection / misleading display) and collapses the
// whitespace ones (tab/newline → single space) so words aren't glued together.
func TestCleanTitle_StripsControlBytes(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ansi-color", "\x1b[31mred\x1b[0m text", "[31mred[0m text"},
		{"bel", "ding\x07dong", "dingdong"},
		{"osc-title-set", "\x1b]0;evil\x07real", "]0;evilreal"},
		{"raw-escape", "a\x1bb", "ab"},
		{"tabs-newlines", "line1\tcol\nline2", "line1 col line2"},
		{"plain", "  hello   world  ", "hello world"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CleanTitle(tc.in)
			if got != tc.want {
				t.Fatalf("CleanTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, r := range got {
				if unicode.IsControl(r) {
					t.Fatalf("CleanTitle(%q) left a control rune %q in %q", tc.in, r, got)
				}
			}
		})
	}
}

// TestLookup_StripsControlBytesFromTranscript proves the sanitizer is applied on
// the read path: a transcript whose customTitle carries an ANSI/BEL sequence
// yields a control-byte-free title from Lookup.
func TestLookup_StripsControlBytesFromTranscript(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sessionID := "66666666-6666-4666-8666-666666666666"
	path := filepath.Join(cfg, "projects", "repo", sessionID+".jsonl")
	// Use JSON \u-escapes for ESC + BEL — that is how a real transcript stores
	// control bytes inside a JSON string (raw control bytes are invalid JSON).
	// json.Unmarshal decodes them to the actual bytes, which CleanTitle strips.
	line := `{"type":"custom-title","customTitle":"\u001b[31mPWNED\u001b[0m\u0007 build","sessionId":"` + sessionID + `"}`
	writeTranscript(t, path, line)

	got := Lookup(sessionID)
	// Pin the exact sanitized output: ESC/BEL stripped, the visible bytes (the
	// literal "[31m"/"[0m" that followed each stripped ESC, and "PWNED build")
	// preserved — matching CleanTitle. A sanitizer that over-stripped the visible
	// text would still be control-free, so the loop below alone is too weak.
	if want := "[31mPWNED[0m build"; got != want {
		t.Fatalf("Lookup = %q, want %q", got, want)
	}
	for _, r := range got {
		if unicode.IsControl(r) {
			t.Fatalf("Lookup result %q contains control rune %q", got, r)
		}
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\x07") {
		t.Fatalf("Lookup result %q still carries ESC/BEL", got)
	}
}

func TestLookupPrefersLatestCustomTitle(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sessionID := "11111111-1111-4111-8111-111111111111"
	path := filepath.Join(cfg, "projects", sanitizePath("/repo"), sessionID+".jsonl")
	writeTranscript(t, path,
		`{"type":"ai-title","aiTitle":"AI title","sessionId":"`+sessionID+`"}`,
		`{"type":"custom-title","customTitle":"Old Name","sessionId":"`+sessionID+`"}`,
		`{"type":"custom-title","customTitle":"New Name","sessionId":"`+sessionID+`"}`,
	)

	if got := Lookup(sessionID); got != "New Name" {
		t.Fatalf("Lookup = %q, want New Name", got)
	}
}

func TestLookupFallsBackToAITitle(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sessionID := "22222222-2222-4222-8222-222222222222"
	path := filepath.Join(cfg, "projects", "other-project", sessionID+".jsonl")
	writeTranscript(t, path,
		`{"type":"ai-title","aiTitle":"Generated Title","sessionId":"`+sessionID+`"}`,
	)

	if got := Lookup(sessionID); got != "Generated Title" {
		t.Fatalf("Lookup = %q, want Generated Title", got)
	}
}

func TestLookupUsesCurrentProjectCandidate(t *testing.T) {
	cfg := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Chdir(cwd)

	sessionID := "33333333-3333-4333-8333-333333333333"
	path := filepath.Join(cfg, "projects", sanitizePath(cwd), sessionID+".jsonl")
	writeTranscript(t, path,
		`{"type":"custom-title","customTitle":"Current Project","sessionId":"`+sessionID+`"}`,
	)

	if got := Lookup(sessionID); got != "Current Project" {
		t.Fatalf("Lookup = %q, want Current Project", got)
	}
}

func TestResolveSkipsMissingAndWrongSession(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	sessionID := "44444444-4444-4444-8444-444444444444"
	path := filepath.Join(cfg, "projects", "repo", sessionID+".jsonl")
	writeTranscript(t, path,
		`{"type":"custom-title","customTitle":"Wrong","sessionId":"55555555-5555-4555-8555-555555555555"}`,
		`{"type":"user","message":{"content":"custom-title should not be parsed from body"},"sessionId":"`+sessionID+`"}`,
	)

	got := Resolve([]string{sessionID, "", "missing", sessionID})
	if len(got) != 0 {
		t.Fatalf("Resolve = %#v, want empty map", got)
	}
}

func writeTranscript(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
