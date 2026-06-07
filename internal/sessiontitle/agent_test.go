package sessiontitle

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAgentTranscript writes a fabricated transcript and pins its mtime.
func writeAgentTranscript(t *testing.T, dir, name, content string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFindAgentTranscript: the newest *.jsonl whose head carries BOTH teamName and agentName
// wins; the lead's transcript (teamName only) and other agents' transcripts never match;
// a notBefore past a candidate's mtime prunes it.
func TestFindAgentTranscript(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	cwd := "/proj/alpha"
	dir := filepath.Join(cfg, "projects", sanitizePath(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	mate := `{"type":"attachment","teamName":"t1","agentName":"alice","sessionId":"s1"}` + "\n"
	writeAgentTranscript(t, dir, "lead.jsonl",
		`{"type":"assistant","teamName":"t1","sessionId":"lead"}`+"\n", base.Add(30*time.Minute))
	writeAgentTranscript(t, dir, "other.jsonl",
		`{"type":"attachment","teamName":"t1","agentName":"bob","sessionId":"s2"}`+"\n", base.Add(20*time.Minute))
	old := writeAgentTranscript(t, dir, "old-alice.jsonl", mate, base)
	fresh := writeAgentTranscript(t, dir, "new-alice.jsonl", mate, base.Add(10*time.Minute))

	got, ok := FindAgentTranscript(cwd, "t1", "alice", time.Time{})
	if !ok || got != fresh {
		t.Fatalf("FindAgentTranscript = %q, %v; want the newest match %q", got, ok, fresh)
	}
	// A notBefore between the two matches prunes the older one — a respawned
	// same-named teammate can never resolve to its predecessor's transcript.
	if got, ok := FindAgentTranscript(cwd, "t1", "alice", base.Add(5*time.Minute)); !ok || got != fresh {
		t.Fatalf("notBefore filter: got %q, %v; want %q", got, ok, fresh)
	}
	if _, ok := FindAgentTranscript(cwd, "t1", "alice", base.Add(15*time.Minute)); ok {
		t.Fatal("a notBefore past every candidate must find nothing")
	}
	if _, ok := FindAgentTranscript(cwd, "t1", "carol", time.Time{}); ok {
		t.Fatal("an unknown agent must not match")
	}
	if _, ok := FindAgentTranscript("", "t1", "alice", time.Time{}); ok {
		t.Fatal("an empty cwd must not match")
	}
	if !agentTranscriptMatches(old, "t1", "alice") {
		t.Fatal("the older transcript should still match by head markers")
	}
	if agentTranscriptMatches(filepath.Join(dir, "lead.jsonl"), "t1", "alice") {
		t.Fatal("the lead transcript (no agentName) must not match")
	}
	if agentTranscriptMatches(filepath.Join(dir, "gone.jsonl"), "t1", "alice") {
		t.Fatal("a missing path must not match")
	}
}
