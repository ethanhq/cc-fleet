package sessiontitle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveCodexTitles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	thread := "0199abcd-1234-5678-9abc-def012345678"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "06", "28")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// session_meta, a developer preamble, the AGENTS.md user preamble, then the real first prompt
	// (multi-line, to confirm whitespace collapse), then an assistant reply.
	lines := []string{
		`{"payload":{"session_id":"x","cwd":"/proj"}}`,
		`{"payload":{"role":"developer","content":[{"type":"text","text":"<permissions instructions>\nFilesystem sandboxing"}]}}`,
		`{"payload":{"role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /proj\n<INSTRUCTIONS>x</INSTRUCTIONS>"}]}}`,
		`{"payload":{"role":"user","content":[{"type":"input_text","text":"研究一下\n这个项目"}]}}`,
		`{"payload":{"role":"assistant","content":[{"type":"text","text":"好的"}]}}`,
	}
	rollout := filepath.Join(dir, "rollout-2026-06-28T13-46-32-"+thread+".jsonl")
	if err := os.WriteFile(rollout, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := ResolveCodexTitles([]string{
		"codex:" + thread,
		"codex:0199ffff-0000-0000-0000-000000000000", // valid uuid, no rollout → omitted (fail-soft)
		"codex:../../etc/passwd",                     // unsafe id → rejected before any glob
		"claude-sess", "",                            // non-codex / empty → ignored
	})
	if got["codex:"+thread] != "研究一下 这个项目" {
		t.Errorf("title = %q, want %q (preamble skipped, whitespace collapsed)", got["codex:"+thread], "研究一下 这个项目")
	}
	if len(got) != 1 {
		t.Errorf("only the resolved codex id should appear; got %v", got)
	}
}

func TestSafeThread(t *testing.T) {
	if !safeThread("0199abcd-1234-5678-9abc-def012345678") {
		t.Error("a canonical uuid must be accepted")
	}
	for _, bad := range []string{
		"",
		"abc",                                   // too short — would broaden the glob
		"0199abcd",                              // hex but not a full uuid
		"0199abcd-1234-5678-9abc-def01234567",   // 35 chars
		"0199abcd-1234-5678-9abc-def0123456789", // 37 chars
		"../../etc/passwd",                      // path separators
		"0199abcd_1234_5678_9abc_def012345678",  // underscores, not dashes
		"gggggggg-1234-5678-9abc-def012345678",  // non-hex
		"0199abc*-1234-5678-9abc-def012345678",  // glob metacharacter
	} {
		if safeThread(bad) {
			t.Errorf("non-uuid accepted: %q", bad)
		}
	}
}
