package workflow

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

func readEvents(t *testing.T, path string) []eventRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer f.Close()
	var out []eventRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r eventRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func TestEventWriterRoundTripSeqAndNilSafe(t *testing.T) {
	var nilw *eventWriter
	nilw.emit(eventRecord{Kind: "leaf"}) // nil writer must be a no-op, not a panic

	path := filepath.Join(t.TempDir(), "x.events")
	w := newEventWriter(path)
	w.emit(eventRecord{Kind: "phase", Phase: "map"})
	w.emit(eventRecord{Kind: "leaf", Status: "launch", Label: "a"})
	recs := readEvents(t, path)
	if len(recs) != 2 {
		t.Fatalf("got %d events, want 2", len(recs))
	}
	if recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Errorf("seq not monotonic: %d, %d", recs[0].Seq, recs[1].Seq)
	}
	if recs[0].Kind != "phase" || recs[1].Status != "launch" {
		t.Errorf("records out of shape: %+v", recs)
	}
}

// TestEngineEmitsLiveEvents: a real run emits phase/log/leaf-launch/leaf-done events,
// and the events file NEVER contains the leaf's answer text (events are key-safe; the
// answer lives only in the journal + the opt-in io files, never the live channel).
func TestEngineEmitsLiveEvents(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec) // returns "ok:<prompt>"
	t.Cleanup(func() { runLeaf = old })

	dir := t.TempDir()
	script := filepath.Join(dir, "e.star")
	src := `meta = {"name": "n", "description": "d"}
phase("map")
log("starting")
x = agent("a", vendor="v", label="alpha")
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	ep, _ := subagent.RunEventsPath(run.RunID)
	recs := readEvents(t, ep)
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Kind+":"+r.Status] = true
	}
	for _, want := range []string{"phase:", "log:", "leaf:launch", "leaf:done"} {
		if !seen[want] {
			t.Errorf("missing event %q; got %v", want, seen)
		}
	}
	data, _ := os.ReadFile(ep)
	if strings.Contains(string(data), "ok:a") {
		t.Error("the live-event channel must never contain the leaf answer text")
	}
}

func eventStatuses(t *testing.T, runID string) map[string]bool {
	t.Helper()
	ep, _ := subagent.RunEventsPath(runID)
	seen := map[string]bool{}
	for _, r := range readEvents(t, ep) {
		seen[r.Kind+":"+r.Status] = true
	}
	return seen
}

// TestEventsCachedAndFailed: a resumed leaf emits leaf:cached (served from the journal),
// and a failing leaf emits leaf:failed.
func TestEventsCachedAndFailed(t *testing.T) {
	t.Run("cached", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		rec := &recorder{}
		old := runLeaf
		runLeaf = echoLeaf(rec)
		t.Cleanup(func() { runLeaf = old })
		_, script := writeScript(t, `x = agent("q", vendor="v")`)
		run, err := Prepare(script)
		if err != nil {
			t.Fatal(err)
		}
		if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
			t.Fatalf("run1: %v", err)
		}
		if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil { // resume → cached
			t.Fatalf("resume: %v", err)
		}
		if !eventStatuses(t, run.RunID)["leaf:cached"] {
			t.Error("a resumed leaf must emit a leaf:cached event")
		}
	})
	t.Run("failed", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		old := runLeaf
		runLeaf = func(subagent.Request) subagent.Result {
			return subagent.Result{OK: false, ErrorCode: "X", ErrorMsg: "boom"}
		}
		t.Cleanup(func() { runLeaf = old })
		_, script := writeScript(t, `x = agent("q", vendor="v")`)
		run, err := Prepare(script)
		if err != nil {
			t.Fatal(err)
		}
		_ = Execute(context.Background(), script, run.RunID, Options{}) // top-level failure → error (expected)
		if !eventStatuses(t, run.RunID)["leaf:failed"] {
			t.Error("a failing leaf must emit a leaf:failed event")
		}
	})
}
