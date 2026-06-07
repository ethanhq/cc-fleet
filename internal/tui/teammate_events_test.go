package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTeammateInbox: entries parse with their read flags; a malformed name, a missing
// file, and unparseable JSON all degrade to an empty inbox.
func TestReadTeammateInbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "teams", "t1", "inboxes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `[{"from":"team-lead","text":"hello","timestamp":"2026-06-07T02:31:00.000Z","type":"message","read":false},
	          {"from":"bob","text":"done","timestamp":"2026-06-07T02:11:00.000Z","type":"message","read":true}]`
	if err := os.WriteFile(filepath.Join(dir, "alice.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := readTeammateInbox("t1", "alice")
	if len(entries) != 2 || entries[0].From != "team-lead" || entries[0].Read || !entries[1].Read {
		t.Fatalf("inbox = %+v", entries)
	}
	if got := readTeammateInbox("t1", "../alice"); got != nil {
		t.Fatalf("a path-unsafe name must read nothing, got %+v", got)
	}
	if got := readTeammateInbox("t1", "missing"); got != nil {
		t.Fatalf("a missing inbox must be empty, got %+v", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readTeammateInbox("t1", "bad"); got != nil {
		t.Fatalf("an unparseable inbox must be empty, got %+v", got)
	}
}

// TestInboxPreview: a native task payload (JSON blob) lifts its subject; plain text shows
// its first line.
func TestInboxPreview(t *testing.T) {
	if got := inboxPreview(`{"type":"task_assignment","subject":"调研 packages","description":"…"}`); got != "调研 packages" {
		t.Fatalf("subject extraction = %q", got)
	}
	if got := inboxPreview("first line\nsecond line"); got != "first line" {
		t.Fatalf("first-line fallback = %q", got)
	}
	if got := inboxPreview(`{"no":"subject"}`); got != `{"no":"subject"}` {
		t.Fatalf("subject-less JSON should fall back to the raw first line, got %q", got)
	}
}

// TestParseTeammateMessage: the transcript wrapper yields from/summary/body; a wrapper
// without a summary attr previews its body (subject lift included); non-wrapper text is
// rejected.
func TestParseTeammateMessage(t *testing.T) {
	msg, ok := parseTeammateMessage("<teammate-message teammate_id=\"team-lead\" summary=\"调研路线图\">\n正文第一行\n第二行\n</teammate-message>", "2026-06-07T08:54:03Z")
	if !ok || msg.from != "team-lead" || msg.summary != "调研路线图" || msg.body != "正文第一行\n第二行" || msg.ts != "2026-06-07T08:54:03Z" {
		t.Fatalf("parsed = %+v, %v", msg, ok)
	}
	msg, ok = parseTeammateMessage(`<teammate-message teammate_id="lead" color="yellow">`+"\n"+`{"type":"task_assignment","subject":"任务七"}`+"\n</teammate-message>", "")
	if !ok || msg.summary != "任务七" {
		t.Fatalf("summary-less wrapper should lift the body subject, got %+v", msg)
	}
	if _, ok := parseTeammateMessage("plain user text", ""); ok {
		t.Fatal("non-wrapper text must not parse as a message")
	}
}

// TestReadTeammateTranscript: one streaming pass projects tool signatures, output messages
// (text blocks per assistant line, chronological), received messages, and the token
// aggregates (↑ max input+cache-read, ↓ summed output); an unreadable path reports
// ok=false.
func TestReadTeammateTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.jsonl")
	body := `{"type":"attachment","teamName":"t1","agentName":"alice"}
{"type":"user","timestamp":"2026-06-07T08:54:03Z","message":{"content":"<teammate-message teammate_id=\"team-lead\" summary=\"调研路线图\">\n任务正文\n</teammate-message>"}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"ignored tool result"}]}}
{"type":"assistant","timestamp":"2026-06-07T08:55:00Z","message":{"content":[{"type":"text","text":"thinking…"},{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/a.go"}}],"usage":{"input_tokens":257,"output_tokens":303,"cache_read_input_tokens":52338}}}
{"type":"assistant","timestamp":"2026-06-07T08:56:00Z","message":{"content":[{"type":"tool_use","name":"TaskList","input":{}},{"type":"text","text":"final summary"}],"usage":{"input_tokens":12430,"output_tokens":222,"cache_read_input_tokens":79602}}}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, ok := readTeammateTranscript(path)
	if !ok {
		t.Fatal("a readable transcript must report ok")
	}
	if got := snap.activity.sigs; len(got) != 2 || got[0] != "Read(/tmp/a.go)" || got[1] != "TaskList({})" {
		t.Fatalf("sigs = %v", got)
	}
	if len(snap.outputs) != 2 || snap.outputs[0].text != "thinking…" || snap.outputs[1].text != "final summary" ||
		snap.outputs[1].ts != "2026-06-07T08:56:00Z" {
		t.Fatalf("outputs = %+v", snap.outputs)
	}
	if len(snap.msgs) != 1 || snap.msgs[0].from != "team-lead" || snap.msgs[0].summary != "调研路线图" || snap.msgs[0].pending {
		t.Fatalf("msgs = %+v", snap.msgs)
	}
	if snap.ctxTok != 12430+79602 || snap.outTok != 303+222 {
		t.Fatalf("tokens = ↑%d ↓%d, want ↑%d ↓%d", snap.ctxTok, snap.outTok, 12430+79602, 303+222)
	}
	if _, ok := readTeammateTranscript(filepath.Join(dir, "missing.jsonl")); ok {
		t.Fatal("a missing transcript must report ok=false")
	}
}

// TestReadTeammateTranscriptSkipsOversizedLine: a line over the in-memory cap is drained
// and skipped — the scan continues and everything after it still projects.
func TestReadTeammateTranscriptSkipsOversizedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.jsonl")
	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxTranscriptLine+1024) + `"}}`
	body := `{"type":"assistant","timestamp":"2026-06-07T08:55:00Z","message":{"content":[{"type":"text","text":"before"}],"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n" +
		huge + "\n" +
		`{"type":"assistant","timestamp":"2026-06-07T08:56:00Z","message":{"content":[{"type":"text","text":"after"}],"usage":{"input_tokens":20,"output_tokens":7}}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, ok := readTeammateTranscript(path)
	if !ok || len(snap.outputs) != 2 || snap.outputs[1].text != "after" || snap.outTok != 12 {
		t.Fatalf("the scan must survive an oversized line: ok=%v outputs=%+v outTok=%d", ok, snap.outputs, snap.outTok)
	}
}
