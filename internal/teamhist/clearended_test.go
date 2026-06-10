package teamhist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/pinned"
)

func writeRec(t *testing.T, rec Record) {
	t.Helper()
	dir, err := historyDir()
	if err != nil {
		t.Fatalf("historyDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, rec.Team+".json"), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestClearEnded_AllMembersMatch: ClearEnded deletes only the records wholly owned by the target
// session, keeps a mixed-session record, and keeps a pinned team (acceptance 4).
func TestClearEnded_AllMembersMatch(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	now := time.Now().UTC().Format(time.RFC3339)

	writeRec(t, Record{Team: "alpha", LastSeen: now, Members: []MemberRec{{Name: "w1", LeadSessionID: "sessA", SpawnTime: 1}}})
	writeRec(t, Record{Team: "beta", LastSeen: now, Members: []MemberRec{
		{Name: "w1", LeadSessionID: "sessA"}, {Name: "w2", LeadSessionID: "sessB"},
	}})
	writeRec(t, Record{Team: "gamma", LastSeen: now, Members: []MemberRec{{Name: "w1", LeadSessionID: "sessA"}}})
	if err := pinned.Pin(pinned.Team, "gamma"); err != nil {
		t.Fatalf("Pin: %v", err)
	}

	pins, err := pinned.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	n, err := ClearEnded("sessA", pins)
	if err != nil {
		t.Fatalf("ClearEnded: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (alpha only)", n)
	}

	recs, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.Team] = true
	}
	if got["alpha"] {
		t.Error("alpha (wholly sessA, unpinned) should be deleted")
	}
	if !got["beta"] {
		t.Error("a mixed-session record must be kept")
	}
	if !got["gamma"] {
		t.Error("a pinned team must be kept")
	}
}

func TestClearEnded_RequiresSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if _, err := ClearEnded("", pinned.Set{}); err == nil {
		t.Error("ClearEnded with empty session id should error")
	}
}

// TestDelete_ClearsPin: deleting a team record drops its pin marker, so a same-name respawn
// doesn't inherit a stale pin.
func TestDelete_ClearsPin(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	writeRec(t, Record{Team: "alpha", LastSeen: time.Now().UTC().Format(time.RFC3339),
		Members: []MemberRec{{Name: "w1", LeadSessionID: "sessA"}}})
	if err := pinned.Pin(pinned.Team, "alpha"); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if err := Delete("alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s, _ := pinned.Snapshot(); s.Has(pinned.Team, "alpha") {
		t.Error("Delete should clear the team's pin marker")
	}
}
