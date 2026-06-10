package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// eventsJSONL marshals records to the on-disk newline-delimited format.
func eventsJSONL(t *testing.T, recs ...EventRecord) string {
	t.Helper()
	var b strings.Builder
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestParseEventRecords: complete lines parse, blanks/garbage are skipped, a torn trailing
// line is carried as the partial.
func TestParseEventRecords(t *testing.T) {
	chunk := eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "phase", Phase: "Build"},
		EventRecord{Seq: 2, Kind: "leaf", Status: "done", Label: "a"},
	) + "\n" + "{not json}\n" + `{"seq":3,"kind":"leaf"`
	evs, partial := parseEventRecords(chunk)
	if len(evs) != 2 {
		t.Fatalf("parsed %d events, want 2 (garbage + blank skipped)", len(evs))
	}
	if evs[0].Seq != 1 || evs[1].Seq != 2 {
		t.Errorf("seqs = %d,%d want 1,2", evs[0].Seq, evs[1].Seq)
	}
	if partial != `{"seq":3,"kind":"leaf"` {
		t.Errorf("partial = %q, want the torn trailing line", partial)
	}
}

// TestEventTailIncremental: a tail reads only the bytes appended since the prior read.
func TestEventTailIncremental(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.events"
	if err := os.WriteFile(path, []byte(eventsJSONL(t, EventRecord{Seq: 1, Kind: "leaf", Label: "a"})), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := &EventTail{}
	evs, reset := tail.read(path)
	if len(evs) != 1 || reset {
		t.Fatalf("first read: %d evs, reset=%v; want 1, false", len(evs), reset)
	}
	// Append one more line; the next read returns only it.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(eventsJSONL(t, EventRecord{Seq: 2, Kind: "leaf", Label: "b"}))
	f.Close()
	evs, reset = tail.read(path)
	if len(evs) != 1 || evs[0].Seq != 2 || reset {
		t.Fatalf("incremental read: %d evs (seq %v), reset=%v; want 1 (seq 2), false", len(evs), seqsOf(evs), reset)
	}
}

// TestEventTailShrinkReset: the common resume — the rewritten generation is smaller than the
// consumed offset at poll time, so the shrink (offset past EOF) triggers a reset + full re-read.
func TestEventTailShrinkReset(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.events"
	if err := os.WriteFile(path, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "leaf", Label: "g1-a"},
		EventRecord{Seq: 2, Kind: "leaf", Label: "g1-b"},
		EventRecord{Seq: 3, Kind: "leaf", Label: "g1-c"},
		EventRecord{Seq: 4, Kind: "leaf", Label: "g1-d"},
		EventRecord{Seq: 5, Kind: "leaf", Label: "g1-e"},
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := &EventTail{}
	if evs, _ := tail.read(path); len(evs) != 5 {
		t.Fatalf("gen1: %d evs, want 5", len(evs))
	}
	// Resume: remove + recreate, smaller than the prior offset → shrink-detected reset.
	os.Remove(path)
	if err := os.WriteFile(path, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "leaf", Label: "g2-a"},
		EventRecord{Seq: 2, Kind: "leaf", Label: "g2-b"},
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, reset := tail.read(path)
	if !reset || len(evs) != 2 || evs[0].Seq != 1 {
		t.Fatalf("after a shrinking rewrite: reset=%v, %d evs starting seq %d; want reset, 2, seq 1", reset, len(evs), firstSeq(evs))
	}
}

// TestEventTailGrownPastReset: the fast-resume race — the rewritten generation has already grown
// PAST the old byte offset (so the shrink check can't fire) AND the freed inode was reused (so
// os.SameFile reports "same"). Seq-contiguity catches it: reading from the stale offset yields a
// line whose Seq does not continue the cursor, so the tailer resets and re-reads from the top.
func TestEventTailGrownPastReset(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.events"
	// Generation 1: ONE event with a long label → a large consumed offset, small cursor (1).
	if err := os.WriteFile(path, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "leaf", Label: strings.Repeat("g1", 160)},
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	tail := &EventTail{}
	if evs, _ := tail.read(path); len(evs) != 1 {
		t.Fatalf("gen1: %d evs, want 1", len(evs))
	}
	// Generation 2: many short events, total grown PAST the gen1 offset. Reading from that
	// offset lands many lines deep, so the first parsed Seq (large) cannot equal cursor+1 (=2).
	var recs []EventRecord
	for i := int64(1); i <= 30; i++ {
		recs = append(recs, EventRecord{Seq: i, Kind: "leaf", Label: "g2"})
	}
	os.Remove(path)
	if err := os.WriteFile(path, []byte(eventsJSONL(t, recs...)), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, reset := tail.read(path)
	if !reset {
		t.Fatal("a grown-past rewrite must be detected as a generation reset (seq-contiguity)")
	}
	if len(evs) != 30 || evs[0].Seq != 1 {
		t.Fatalf("after reset: %d evs starting seq %d; want the full new generation (30 from seq 1)", len(evs), firstSeq(evs))
	}
}

// TestRenderEventScrubsControl: a control sequence in an opaque field never reaches the line.
func TestRenderEventScrubsControl(t *testing.T) {
	out := RenderEventLine(EventRecord{Kind: "leaf", Status: "done", Label: "a\x1b[31mb", Provider: "v", Model: "m"})
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("render leaked an ESC control rune: %q", out)
	}
	if !strings.Contains(out, "done") || !strings.Contains(out, "v/m") {
		t.Errorf("render dropped expected fields: %q", out)
	}
}

// TestWatchTerminalRun: a run already terminal streams its events, prints a final status line,
// and returns nil. A failed run's final line points at `workflow status` and never echoes the
// raw Error (which a schema-reject can taint with a provider reply).
func TestWatchTerminalRun(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := subagent.RunEventsPath(run.RunID)
	if err := os.WriteFile(ep, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "phase", Phase: "Build"},
		EventRecord{Seq: 2, Kind: "leaf", Status: "done", Label: "worker"},
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	run.Status = "failed"
	run.Error = "agent(v): schema not satisfied: value \"LEAKED_PROVIDER_REPLY\" is not one of the enum values"
	if err := subagent.SaveRun(run); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	got, werr := Watch(context.Background(), run.RunID, &buf, WatchOptions{Interval: 5 * time.Millisecond})
	if werr != nil {
		t.Fatalf("Watch returned %v, want nil for a terminal run", werr)
	}
	if got.Status != "failed" {
		t.Errorf("returned status %q, want failed", got.Status)
	}
	s := buf.String()
	if !strings.Contains(s, "phase Build") || !strings.Contains(s, "leaf done") {
		t.Errorf("stream missing events:\n%s", s)
	}
	if !strings.Contains(s, "failed") || !strings.Contains(s, "workflow status") {
		t.Errorf("final line should mark failed + point at `workflow status`:\n%s", s)
	}
	if strings.Contains(s, "LEAKED_PROVIDER_REPLY") {
		t.Errorf("the raw WorkflowRun.Error (tainted with a provider reply) leaked to stdout:\n%s", s)
	}
}

// TestWatchEngineGone: a stale "running" manifest whose detached engine is dead returns
// ErrEngineGone rather than blocking forever.
func TestWatchEngineGone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = "running"
	run.EnginePID = 0x7ffffffe // a pid that is not us and (effectively) not alive
	if err := subagent.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	// An event the dying engine left behind must still be drained before exiting.
	ep, _ := subagent.RunEventsPath(run.RunID)
	if err := os.WriteFile(ep, []byte(eventsJSONL(t, EventRecord{Seq: 1, Kind: "leaf", Status: "done", Label: "last"})), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, werr := Watch(ctx, run.RunID, &buf, WatchOptions{Interval: 5 * time.Millisecond})
	if werr != ErrEngineGone {
		t.Fatalf("Watch returned %v, want ErrEngineGone for a dead detached engine", werr)
	}
	s := buf.String()
	if !strings.Contains(s, "engine gone") {
		t.Errorf("missing engine-gone line:\n%s", s)
	}
	if !strings.Contains(s, "last") {
		t.Errorf("a final event must be drained before the engine-gone exit:\n%s", s)
	}
}

// TestWatchContextCancel: a still-running foreground run (EnginePID 0) blocks until ctx, then
// prints a "still running (seq=N)" reattach line and returns the ctx error.
func TestWatchContextCancel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, err := subagent.NewRunWithMeta("n", "d", "", nil) // Status running, EnginePID 0
	if err != nil {
		t.Fatal(err)
	}
	ep, _ := subagent.RunEventsPath(run.RunID)
	if err := os.WriteFile(ep, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "leaf", Status: "launch", Label: "w"},
		EventRecord{Seq: 2, Kind: "leaf", Status: "done", Label: "w"},
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, werr := Watch(ctx, run.RunID, &buf, WatchOptions{Interval: 5 * time.Millisecond})
	if werr != context.DeadlineExceeded {
		t.Fatalf("Watch returned %v, want context.DeadlineExceeded", werr)
	}
	if !strings.Contains(buf.String(), "still running (seq=2)") {
		t.Errorf("missing reattach line with the last seq:\n%s", buf.String())
	}
}

// TestWatchSinceSeq: --since-seq skips already-seen events (clean reattach).
func TestWatchSinceSeq(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	run, _ := subagent.NewRunWithMeta("n", "d", "", nil)
	ep, _ := subagent.RunEventsPath(run.RunID)
	os.WriteFile(ep, []byte(eventsJSONL(t,
		EventRecord{Seq: 1, Kind: "leaf", Status: "done", Label: "old"},
		EventRecord{Seq: 2, Kind: "leaf", Status: "done", Label: "new"},
	)), 0o600)
	run.Status = "done"
	subagent.SaveRun(run)

	var buf bytes.Buffer
	if _, err := Watch(context.Background(), run.RunID, &buf, WatchOptions{Interval: 5 * time.Millisecond, SinceSeq: 1}); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if strings.Contains(s, "old") {
		t.Errorf("--since-seq 1 should skip seq 1 (\"old\"):\n%s", s)
	}
	if !strings.Contains(s, "new") {
		t.Errorf("--since-seq 1 should still show seq 2 (\"new\"):\n%s", s)
	}
}

// TestTailEvents exercises the value-threaded façade the board uses: a zero EventTail reads the
// whole file (reset=false), the returned tail reads only newly-appended events, and an absent
// file degrades to no events.
func TestTailEvents(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.events"
	if err := os.WriteFile(path, []byte(eventsJSONL(t, EventRecord{Seq: 1, Kind: "leaf", Label: "a"})), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, tail, reset := TailEvents(path, EventTail{})
	if len(evs) != 1 || reset {
		t.Fatalf("first read: %d evs, reset=%v; want 1, false", len(evs), reset)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(eventsJSONL(t, EventRecord{Seq: 2, Kind: "leaf", Label: "b"}))
	f.Close()
	evs, _, reset = TailEvents(path, tail)
	if len(evs) != 1 || evs[0].Seq != 2 || reset {
		t.Fatalf("incremental: %d evs (seq %v), reset=%v; want 1 (seq 2), false", len(evs), seqsOf(evs), reset)
	}
	if evs, _, reset := TailEvents(dir+"/absent.events", EventTail{}); len(evs) != 0 || reset {
		t.Fatalf("absent file: %d evs, reset=%v; want 0, false", len(evs), reset)
	}
}

func seqsOf(evs []EventRecord) []int64 {
	var out []int64
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func firstSeq(evs []EventRecord) int64 {
	if len(evs) == 0 {
		return -1
	}
	return evs[0].Seq
}
