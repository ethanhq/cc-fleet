package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// watchDefaultInterval is the poll cadence for Watch: tail the events file + re-read the
// manifest. ~500ms keeps the live log responsive without busy-spinning on cheap reads.
// watchMinInterval floors a tiny POSITIVE --interval so it can't busy-spin the per-tick file
// reads (matching cc-fleet watch's floor); opts.Interval <= 0 still means "use the default".
const (
	watchDefaultInterval = 500 * time.Millisecond
	watchMinInterval     = 100 * time.Millisecond
)

// watchResetLine marks a detected generation reset (the run was truncated + rewritten — a
// resume). watchEngineGoneSuffix / watchStillRunning are the terminal/non-terminal final lines.
const watchResetLine = "── run restarted ──"

// ErrEngineGone is returned by Watch when the manifest is still "running" but the run's
// DETACHED engine process is gone (crashed / killed) without finalizing — so waiting for a
// terminal status would block forever. The watcher surfaces it and stops.
var ErrEngineGone = errors.New("workflow: detached engine is gone but the run never finalized")

// WatchOptions configures a workflow-run watch.
type WatchOptions struct {
	// Interval is the poll cadence; <=0 uses watchDefaultInterval.
	Interval time.Duration
	// SinceSeq skips events whose Seq <= SinceSeq, for a clean reattach (the agent passes the
	// last seq from a prior "still running (seq=N)" line so a re-run doesn't replay history).
	SinceSeq int64
}

// Watch streams a run's live events to out, one scrubbed line per event, and blocks until the
// run reaches a terminal status (then a final status line + return nil), the detached engine
// dies without finalizing (ErrEngineGone), or ctx is cancelled/timed-out (a "still running"
// line + ctx.Err()). It is INSPECTION-ONLY: no spawn/stop/teardown — the only writes are the
// dead-job result memoization any status read (the board, `workflow status`) already performs.
//
// Key-safety rests on the field SOURCE, not on scrubbing: the events stream carries no vendor
// KEY by construction (a key never enters the engine's string space — it flows only via the
// apiKeyHelper), and the final line prints only Status, never the raw WorkflowRun.Error (which a
// schema-reject can taint with a vendor reply fragment). Like the board and `workflow status`,
// the stream DOES surface script-authored text (phase / label / log), so a script that
// deliberately logs a vendor reply will show it — that is the author's choice, not an autonomous
// leak. CleanTitle is layered on every opaque string only as a terminal-injection defense.
func Watch(ctx context.Context, runID string, out io.Writer, opts WatchOptions) (subagent.WorkflowRun, error) {
	if err := subagent.ValidateRunID(runID); err != nil {
		return subagent.WorkflowRun{}, err
	}
	run, err := subagent.ReadRun(runID) // unknown id → path-free "not found"
	if err != nil {
		return subagent.WorkflowRun{}, err
	}
	evPath, err := subagent.RunEventsPath(runID)
	if err != nil {
		return run, err
	}
	interval := opts.Interval
	switch {
	case interval <= 0:
		interval = watchDefaultInterval
	case interval < watchMinInterval:
		interval = watchMinInterval
	}
	tail := &EventTail{since: opts.SinceSeq}
	drain := func() {
		evs, reset := tail.read(evPath)
		if reset {
			fmt.Fprintln(out, watchResetLine)
		}
		for _, ev := range evs {
			fmt.Fprintln(out, RenderEventLine(ev))
		}
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		drain()
		if r, rerr := subagent.ReadRun(runID); rerr == nil {
			run = r // keep the last good manifest on a transient read error
		}
		if isTerminalStatus(run.Status) {
			drain() // final drain: catch events written just before the status flip
			fmt.Fprintln(out, finalLine(run))
			return run, nil
		}
		if run.EnginePID != 0 && !subagent.EngineAlive(run) {
			drain() // catch any final events the dying engine appended before exiting
			fmt.Fprintln(out, "run "+sessiontitle.CleanTitle(run.RunID)+" engine gone — did not finish")
			return run, ErrEngineGone
		}
		select {
		case <-ctx.Done():
			// The run may have flipped terminal while we waited — re-read before declaring it
			// still running, so a just-finished run isn't misreported (and the CLI doesn't
			// map a stale "still running" to the timeout exit code).
			drain()
			if r, rerr := subagent.ReadRun(runID); rerr == nil && isTerminalStatus(r.Status) {
				drain()
				fmt.Fprintln(out, finalLine(r))
				return r, nil
			}
			fmt.Fprintf(out, "run %s still running (seq=%d)\n", sessiontitle.CleanTitle(run.RunID), tail.cursorSeq)
			return run, ctx.Err()
		case <-tick.C:
		}
	}
}

// isTerminalStatus reports whether a manifest status is a final state (anything that is set
// and not "running": done | failed | stopped).
func isTerminalStatus(status string) bool { return status != "" && status != "running" }

// finalLine is the closing status line. It prints Status only — never the raw Error (which a
// schema-reject can taint with a vendor reply fragment); a failed run points at `workflow
// status` for the cause. The run id is scrubbed as a terminal-injection defense.
func finalLine(run subagent.WorkflowRun) string {
	s := "run " + sessiontitle.CleanTitle(run.RunID) + " " + sessiontitle.CleanTitle(run.Status)
	if run.Status == "failed" {
		s += " — run `cc-fleet workflow status " + sessiontitle.CleanTitle(run.RunID) + "` for the cause"
	}
	return s
}

// RenderEventLine formats one event for the live stream. Every opaque field (status / label /
// vendor / model / phase / msg / group type) is CleanTitle-scrubbed before it reaches the
// terminal; the event carries no key or answer by construction (events.go). It is the renderer for
// the `cc-fleet workflow watch` live status stream.
func RenderEventLine(ev EventRecord) string {
	clean := sessiontitle.CleanTitle
	switch ev.Kind {
	case "leaf":
		s := "  leaf " + clean(ev.Status) + " · " + clean(ev.Label)
		if ev.Vendor != "" || ev.Model != "" {
			s += " (" + clean(ev.Vendor) + "/" + clean(ev.Model) + ")"
		}
		return s
	case "phase":
		s := "phase " + clean(ev.Phase)
		if ev.Msg != "" {
			s += " — " + clean(ev.Msg)
		}
		return s
	case "group-open":
		return "  ▸ " + clean(ev.GroupTy)
	case "group-close":
		return "  ◂ end"
	default: // log + anything else
		if ev.Msg != "" {
			return "  " + clean(ev.Msg)
		}
		return "  " + clean(ev.Kind)
	}
}

// EventTail is the incremental-tail state for a run's events file: the byte offset already
// consumed, a torn trailing partial line carried across reads, the identity of the generation
// being tailed, the seq of the last consumed event (contiguity check + reattach hint), and a
// one-time skip threshold (SinceSeq, for a clean reattach).
//
// Watch holds a long-lived *EventTail and calls read directly; the TUI board, which threads
// per-run tail state by value through its refresh loop, drives the same logic via TailEvents.
type EventTail struct {
	offset    int64
	partial   string
	info      os.FileInfo // generation identity (os.SameFile); nil before the first read
	cursorSeq int64       // seq of the last consumed event; the reattach hint
	since     int64       // skip-PRINTING events with Seq <= since (cleared on a generation reset)
}

// TailEvents is the value-threaded façade over (*EventTail).read for a caller that keeps per-run
// tail state in a map passed through its own refresh loop (the TUI board): it advances a COPY of
// prev and returns the new events, the advanced tail to store back, and whether a generation
// reset was detected. The caller passes the zero EventTail on the first read. (Watch instead
// holds a *EventTail across its loop and calls read directly.)
func TailEvents(path string, prev EventTail) (evs []EventRecord, next EventTail, reset bool) {
	evs, reset = (&prev).read(path)
	return evs, prev, reset
}

// read returns the events appended since the last read that are PRINTABLE (Seq > since), plus
// whether a generation RESET was detected. The engine truncates the events file by REMOVING +
// recreating it at the start of every run/resume, so a reset is detected two ways: (1)
// os.SameFile — the file identity changed (or an in-place shrink past EOF); this catches a
// rewrite cheaply UNLESS the freed inode was reused; (2) Seq-contiguity — a generation's events
// are seq 1,2,3,… with no gaps (engine-guaranteed), so the first event of a read must continue
// the cursor; if not, the file was rewritten even when an inode was reused AND the new
// generation already grew past the old byte offset (the fast-resume race). On reset the skip
// threshold clears (the new generation shows in full) and the cursor restarts. A
// missing/unreadable file degrades to no new events (a just-removed file mid-rewrite yields
// nothing until the new one appears).
//
// Known limitation: all three signals can alias at once — a reused inode AND a grown-past
// generation AND a byte-exact line+seq alignment that makes the stale offset's first line read
// seq == cursorSeq+1. The new generation's head lines are then dropped from the live stream
// (no banner). This is OBSERVABILITY ONLY: events are never read back by the engine and never
// journaled, so resume determinism is untouched and the manifest + `workflow status` stay
// authoritative; the coincidence is rare.
func (t *EventTail) read(path string) (printable []EventRecord, reset bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	if (t.info != nil && !os.SameFile(t.info, info)) || t.offset > info.Size() {
		t.resetState()
		reset = true
	}
	t.info = info
	parsed := t.readFrom(f, info.Size())
	if len(parsed) > 0 && parsed[0].Seq != t.cursorSeq+1 {
		t.resetState()
		reset = true
		parsed = t.readFrom(f, info.Size())
	}
	for _, ev := range parsed {
		if ev.Seq > t.cursorSeq {
			t.cursorSeq = ev.Seq
		}
		if ev.Seq > t.since {
			printable = append(printable, ev)
		}
	}
	return printable, reset
}

// readFrom reads the bytes [t.offset, size) from f, parses the complete lines (carrying a torn
// partial across reads), and advances t.offset/t.partial. The caller holds f open and supplies
// its current size.
func (t *EventTail) readFrom(f *os.File, size int64) []EventRecord {
	if t.offset >= size {
		return nil
	}
	if _, err := f.Seek(t.offset, 0); err != nil {
		return nil
	}
	buf := make([]byte, size-t.offset)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil
	}
	parsed, partial := parseEventRecords(t.partial + string(buf[:n]))
	t.offset += int64(n)
	t.partial = partial
	return parsed
}

// resetState rewinds the tail to the top of a fresh generation, clearing the reattach skip
// threshold so the new generation is shown in full.
func (t *EventTail) resetState() {
	t.offset = 0
	t.partial = ""
	t.cursorSeq = 0
	t.since = 0
}

// parseEventRecords splits chunk into complete newline-terminated lines, unmarshals each into
// an EventRecord (silently skipping a line that doesn't parse), and returns the parsed events
// plus the trailing partial (text after the last newline) to carry into the next read. Pure:
// table-tested independently of the filesystem.
func parseEventRecords(chunk string) ([]EventRecord, string) {
	var out []EventRecord
	for {
		i := strings.IndexByte(chunk, '\n')
		if i < 0 {
			break
		}
		line := chunk[:i]
		chunk = chunk[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev EventRecord
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, chunk
}
