package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJournalKeyDeterministicAndDistinct(t *testing.T) {
	base := journalKey("v", "m", "prompt", `{"required":["x"]}`, "", "", nil, false, false)
	if base != journalKey("v", "m", "prompt", `{"required":["x"]}`, "", "", nil, false, false) {
		t.Error("same inputs must hash to the same key (determinism is the whole point)")
	}
	// Each component changes the key.
	cases := map[string]string{
		"vendor":    journalKey("v2", "m", "prompt", `{"required":["x"]}`, "", "", nil, false, false),
		"model":     journalKey("v", "m2", "prompt", `{"required":["x"]}`, "", "", nil, false, false),
		"prompt":    journalKey("v", "m", "prompt2", `{"required":["x"]}`, "", "", nil, false, false),
		"schema":    journalKey("v", "m", "prompt", `{"required":["y"]}`, "", "", nil, false, false),
		"isolation": journalKey("v", "m", "prompt", `{"required":["x"]}`, "worktree", "", nil, false, false),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("changing %s must change the key", name)
		}
	}
	// No framing collision: ("a","b",..) must not equal ("ab","",..).
	if journalKey("a", "b", "p", "", "", "", nil, false, false) == journalKey("ab", "", "p", "", "", "", nil, false, false) {
		t.Error("component boundaries must be unambiguous (length-prefixed framing)")
	}
}

func TestJournalLoadMissingIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.journal")
	j := loadJournal(path)
	if _, ok := j.lookup("anything"); ok {
		t.Error("a missing journal must yield an empty cache")
	}
	// An append persists to disk (the NEXT load sees it) but must NOT feed the current
	// run's own replay lookups — replay serves prior-run results only, so a duplicate
	// call within this run still executes.
	j.append("k1", "r1")
	if _, ok := j.lookup("k1"); ok {
		t.Error("append must not memoize against the current run's own lookups")
	}
	if r, ok := loadJournal(path).lookup("k1"); !ok || r != "r1" {
		t.Errorf("a fresh reload must see the appended entry, got %q,%v want r1,true", r, ok)
	}
}

func TestJournalPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.journal")
	j := loadJournal(path)
	j.append("k1", "r1")
	j.append("k2", `{"a":1}`)
	// A fresh load from disk must see both entries.
	j2 := loadJournal(path)
	if r, ok := j2.lookup("k1"); !ok || r != "r1" {
		t.Errorf("reloaded k1 = %q,%v want r1,true", r, ok)
	}
	if r, ok := j2.lookup("k2"); !ok || r != `{"a":1}` {
		t.Errorf("reloaded k2 = %q,%v", r, ok)
	}
	if _, ok := j2.lookup("absent"); ok {
		t.Error("absent key must miss")
	}
}

func TestJournalSkipsMalformedAndTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.journal")
	// A good line, a garbage line, a good line, then a torn final line (no newline).
	content := `{"key":"good1","result":"r1"}` + "\n" +
		`not json at all` + "\n" +
		`{"key":"good2","result":"r2"}` + "\n" +
		`{"key":"torn","resul`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	j := loadJournal(path)
	for _, k := range []string{"good1", "good2"} {
		if _, ok := j.lookup(k); !ok {
			t.Errorf("valid entry %q must survive a malformed/torn neighbor", k)
		}
	}
	if _, ok := j.lookup("torn"); ok {
		t.Error("a torn final line must be skipped, not partially applied")
	}
}

func TestJournalRepeatedKeyFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.journal")
	j := loadJournal(path)
	j.append("k", "first")
	j.append("k", "second")
	// A reload sees both entries as a FIFO queue; each lookup consumes one in
	// journaled (original execution) order, and a third lookup misses (so a third
	// duplicate call would re-run rather than re-serve a stale entry).
	r := loadJournal(path)
	if v, ok := r.lookup("k"); !ok || v != "first" {
		t.Errorf("first lookup = %q,%v, want first,true (FIFO order)", v, ok)
	}
	if v, ok := r.lookup("k"); !ok || v != "second" {
		t.Errorf("second lookup = %q,%v, want second,true", v, ok)
	}
	if _, ok := r.lookup("k"); ok {
		t.Error("third lookup must miss (queue exhausted → the call re-runs)")
	}
}

func TestJournalNilSafe(t *testing.T) {
	var j *journal // an engine built without a journal (leaf unit tests)
	if _, ok := j.lookup("k"); ok {
		t.Error("nil journal lookup must miss, not panic")
	}
	j.append("k", "r") // must not panic
}

// TestRemoveJournalKey: dropping a key removes ALL its entries (interchangeable duplicate leaves
// re-run together — FIFO-safe), leaves other keys intact, and no-ops on an absent key / missing file.
func TestRemoveJournalKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.journal")
	lines := `{"key":"keyA","result":"a1"}
{"key":"keyB","result":"b1"}
{"key":"keyA","result":"a2"}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := removeJournalKey(path, "keyA")
	if err != nil || !removed {
		t.Fatalf("removeJournalKey(keyA) = (%v, %v), want (true, nil)", removed, err)
	}
	j := loadJournal(path)
	if _, ok := j.lookup("keyA"); ok {
		t.Fatal("keyA should be gone after removal")
	}
	if v, ok := j.lookup("keyB"); !ok || v != "b1" {
		t.Fatalf("keyB must survive removal, got (%q, %v)", v, ok)
	}
	if removed, err := removeJournalKey(path, "absent"); removed || err != nil {
		t.Fatalf("removeJournalKey(absent) = (%v, %v), want (false, nil)", removed, err)
	}
	if removed, err := removeJournalKey(filepath.Join(dir, "nope.journal"), "x"); removed || err != nil {
		t.Fatalf("removeJournalKey(missing-file) = (%v, %v), want (false, nil)", removed, err)
	}
}
