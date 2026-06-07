package teamhist

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// sandbox points ConfigDir at a temp dir so records never touch the real config tree.
func sandbox(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := historyDir()
	if err != nil {
		t.Fatalf("historyDir: %v", err)
	}
	return dir
}

func live(team, name string, joinedAt int64, session string) teardown.Teammate {
	return teardown.Teammate{Team: team, Name: name, SpawnTime: joinedAt, LeadSessionID: session}
}

func cwdMap(m map[string]string) func(string) string {
	return func(s string) string { return m[s] }
}

// TestUpsertListRoundtrip: an Upsert writes a record List reads back, with the
// per-member cwd resolved through cwdOf.
func TestUpsertListRoundtrip(t *testing.T) {
	sandbox(t)
	mates := []teardown.Teammate{
		{Team: "alpha", Name: "alice", Vendor: "glm", Model: "glm-4.6", SpawnTime: 1000, LeadSessionID: "s1"},
		{Team: "alpha", Name: "bob", Vendor: "kimi", SpawnTime: 2000, LeadSessionID: "s2"},
	}
	if err := Upsert(mates, cwdMap(map[string]string{"s1": "/work/a", "s2": "/work/b"})); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	recs, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].Team != "alpha" || len(recs[0].Members) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", recs)
	}
	if recs[0].Members[0].Cwd != "/work/a" || recs[0].Members[1].Cwd != "/work/b" {
		t.Errorf("cwd not threaded through cwdOf: %+v", recs[0].Members)
	}
	if recs[0].LastSeen == "" {
		t.Error("LastSeen unset")
	}
}

// TestContentUnchangedSkipsRewrite: a second Upsert with identical content within
// rewriteInterval leaves the file mtime stable (no churn).
func TestContentUnchangedSkipsRewrite(t *testing.T) {
	dir := sandbox(t)
	mates := []teardown.Teammate{live("alpha", "alice", 1000, "s1")}
	if err := Upsert(mates, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	path := filepath.Join(dir, "alpha.json")
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 1: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := Upsert(mates, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 2: %v", err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Errorf("identical content within the interval rewrote the file (mtime %v → %v)", fi1.ModTime(), fi2.ModTime())
	}
}

// TestTombstoneBlocksResurrection: after Delete, an Upsert from a stale board
// observation (no newer JoinedAt) does not re-create the record.
func TestTombstoneBlocksResurrection(t *testing.T) {
	dir := sandbox(t)
	mates := []teardown.Teammate{live("alpha", "alice", 1000, "s1")}
	if err := Upsert(mates, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := Delete("alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); !os.IsNotExist(err) {
		t.Fatalf("record not removed by Delete: %v", err)
	}
	// A stale board re-observes the SAME (old) team — the tombstone must block it.
	if err := Upsert(mates, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert (stale): %v", err)
	}
	recs, _ := List()
	if len(recs) != 0 {
		t.Errorf("tombstone failed to block resurrection: %+v", recs)
	}
}

// TestNewerJoinedAtClearsTombstone: a respawn (a member JoinedAt newer than the
// tombstone) clears the tombstone and re-records the team.
func TestNewerJoinedAtClearsTombstone(t *testing.T) {
	dir := sandbox(t)
	if err := Upsert([]teardown.Teammate{live("alpha", "alice", 1000, "s1")}, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert 1: %v", err)
	}
	if err := Delete("alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// A real respawn: JoinedAt is well past the tombstone's mtime (now-ish in millis).
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := Upsert([]teardown.Teammate{live("alpha", "alice2", future, "s1")}, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert 2: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha"+tombstoneExt)); !os.IsNotExist(err) {
		t.Errorf("tombstone not cleared by respawn: %v", err)
	}
	recs, _ := List()
	if len(recs) != 1 || recs[0].Members[0].Name != "alice2" {
		t.Errorf("respawn did not re-record the team: %+v", recs)
	}
}

// TestDeleteRemovesRecord: Delete removes the record file and leaves a tombstone.
func TestDeleteRemovesRecord(t *testing.T) {
	dir := sandbox(t)
	if err := Upsert([]teardown.Teammate{live("alpha", "alice", 1000, "s1")}, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := Delete("alpha"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha.json")); !os.IsNotExist(err) {
		t.Errorf("record still present after Delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "alpha"+tombstoneExt)); err != nil {
		t.Errorf("tombstone not written: %v", err)
	}
}

// TestPurgeRemovesDir: Purge removes the whole teams-history dir.
func TestPurgeRemovesDir(t *testing.T) {
	dir := sandbox(t)
	if err := Upsert([]teardown.Teammate{live("alpha", "alice", 1000, "s1")}, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := Purge()
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if got != dir {
		t.Errorf("Purge returned %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir still present after Purge: %v", err)
	}
}

// TestListSkipsInvalidNames: a record carrying a path-unsafe member name is
// silently dropped on read (the names feed transcript path joins).
func TestListSkipsInvalidNames(t *testing.T) {
	dir := sandbox(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bad := `{"team":"alpha","last_seen":"2026-06-07T00:00:00Z","members":[{"name":"../escape"}]}`
	if err := os.WriteFile(filepath.Join(dir, "alpha.json"), []byte(bad), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	good := `{"team":"beta","last_seen":"2026-06-07T00:00:00Z","members":[{"name":"ok"}]}`
	if err := os.WriteFile(filepath.Join(dir, "beta.json"), []byte(good), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	recs, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 || recs[0].Team != "beta" {
		t.Errorf("List should drop the invalid-member record, got %+v", recs)
	}
}

// TestPathUnsafeTeamRejected: an Upsert/Delete with a path-traversing team name
// errors before touching the filesystem.
func TestPathUnsafeTeamRejected(t *testing.T) {
	sandbox(t)
	if err := Delete("../evil"); err == nil {
		t.Error("Delete accepted a path-unsafe team name")
	}
	// Upsert is best-effort per-team: a bad name silently writes nothing, never escapes.
	if err := Upsert([]teardown.Teammate{live("../evil", "alice", 1000, "s1")}, cwdMap(nil)); err != nil {
		t.Fatalf("Upsert returned a hard error: %v", err)
	}
	recs, _ := List()
	if len(recs) != 0 {
		t.Errorf("path-unsafe team was recorded: %+v", recs)
	}
}
