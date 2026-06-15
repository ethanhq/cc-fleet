package subagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/redact"
)

// activityRecord is one line of a leaf's activity sidecar (<jobID>.activity, NDJSON, 0600). It is
// PURE OBSERVABILITY + content-privacy (PersistIO-gated, like <id>.prompt/.answer): a "tool" record
// carries the tool name + a masked/truncated preview of the model's first tool input; a "usage"
// record carries a running token snapshot. It NEVER carries the provider key (that flows only through
// the apiKeyHelper, never into claude's stdout) nor a tool_result body nor the final answer.
type activityRecord struct {
	Seq   int64  `json:"seq"`
	Kind  string `json:"kind"`            // "tool" | "usage"
	Tool  string `json:"tool,omitempty"`  // kind=tool: the tool name (e.g. "Bash", "WebFetch")
	Arg   string `json:"arg,omitempty"`   // kind=tool: masked + truncated first input value
	In    int    `json:"in,omitempty"`    // kind=usage: input tokens (running snapshot)
	Out   int    `json:"out,omitempty"`   // kind=usage: output tokens
	Cache int    `json:"cache,omitempty"` // kind=usage: cache-read input tokens
	TS    string `json:"ts,omitempty"`    // RFC3339 UTC (the subagent Go layer — clock allowed)
}

// maxActivityArg bounds a stored tool-arg preview so a long model input can't bloat the sidecar.
const maxActivityArg = 200

// activityWriter appends a leaf's activity records to <jobID>.activity. It mirrors eventWriter:
// one open/append/close per line (no shared fd), 0600, MkdirAll-recreate-safe, best-effort,
// nil-receiver-safe. The seq counter is bumped by a single writer goroutine, so it needs no atomic.
type activityWriter struct {
	path string
	seq  int64
	// inputSeed is a prompt-derived estimate of the leaf's input tokens. consume() seeds the live
	// input figure with it so the board shows a non-zero token count from the first streamed message
	// even for a provider that reports no per-message usage; a real usage.input_tokens supersedes it.
	inputSeed int
}

// newActivityWriter returns a writer for path, or nil when path is empty (no activity capture) — a
// nil writer's methods are all no-ops.
func newActivityWriter(path string) *activityWriter {
	if path == "" {
		return nil
	}
	return &activityWriter{path: path}
}

// freshActivitySidecar empties a prior attempt's .activity that survived the best-effort restart cleanup
// (a failed remove — e.g. a Windows open-handle race), so the new attempt's writer appends to a clean
// file. The board synthesizes a snapshot's attempt from the live meta, so a surviving stale/mixed sidecar
// would be read as the current attempt; truncating before the running meta is published makes the
// sidecar's content provably this attempt's. Truncate-only (no O_CREATE): an absent file is a no-op, so
// it never leaves an orphan when the cleanup already removed the file. Best-effort, nil-path-safe.
func freshActivitySidecar(path string) {
	if path == "" {
		return
	}
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		_ = f.Close()
	}
}

// estimatePromptTokens roughly estimates a prompt's input-token count from its rune count (≈3 runes
// per token), used to seed the live input figure before the provider reports real usage.
func estimatePromptTokens(prompt string) int {
	return utf8.RuneCountInString(prompt) / 3
}

// emit stamps the next seq and appends one record. Nil-safe; best-effort (a write hiccup just drops
// that observability row, never affects the run).
func (w *activityWriter) emit(rec activityRecord) {
	if w == nil {
		return
	}
	w.seq++
	rec.Seq = w.seq
	if rec.TS == "" {
		rec.TS = time.Now().UTC().Format(time.RFC3339)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(w.path), 0o700)
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// activitySink wraps the stdout cappedWriter on the SYNC leaf path: every write goes to the cap
// FIRST (so the byte-cap / overflow→kill timing is byte-identical to a bare cappedWriter), then a
// COPY is handed to a parser goroutine over a bounded channel — dropped on pressure so activity
// capture can NEVER block the os/exec copy goroutine or delay the process-group kill. The parser
// extracts tool_use names + first-arg previews and per-message usage snapshots and writes them to
// the activity sidecar live, so the board sees a sync leaf's tool calls + tokens WHILE it runs.
type activitySink struct {
	cap   *cappedWriter
	lines chan []byte
	done  chan struct{}
}

// activityChanCap bounds the hand-off channel; a flooded channel drops rows (best-effort) rather
// than back-pressuring the capture path.
const activityChanCap = 256

// newActivitySink starts the parser goroutine writing to w and returns the sink wrapping cap.
func newActivitySink(cap *cappedWriter, w *activityWriter) *activitySink {
	s := &activitySink{cap: cap, lines: make(chan []byte, activityChanCap), done: make(chan struct{})}
	go s.consume(w)
	return s
}

// Write tees to the cap first (preserving its (len(p), nil) contract + overflow timing), then
// best-effort hands a copy to the parser; a full channel drops the chunk rather than blocking.
func (s *activitySink) Write(p []byte) (int, error) {
	n, err := s.cap.Write(p)
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case s.lines <- cp:
	default: // parser behind → drop this chunk (activity is best-effort observability)
	}
	return n, err
}

// close stops the parser and waits for it to flush. Call after cmd.Wait() joins the copy goroutine.
func (s *activitySink) close() {
	close(s.lines)
	<-s.done
}

// consume reassembles complete NDJSON lines across chunk boundaries and feeds each to
// parseStreamLine, which accumulates the leaf's running token usage in a streamAccum.
func (s *activitySink) consume(w *activityWriter) {
	defer close(s.done)
	var buf []byte
	a := &streamAccum{}
	if w != nil {
		a.inTok = w.inputSeed // start input at the prompt estimate; a real usage.input_tokens supersedes it
	}
	for chunk := range s.lines {
		buf = append(buf, chunk...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			parseStreamLine(buf[:i], w, a)
			buf = buf[i+1:]
		}
	}
}

// streamLine is the LENIENT view of a stream-json NDJSON line — only the fields activity capture
// needs. With --include-partial-messages claude wraps the raw Anthropic SSE in type:"stream_event":
// message_start carries the input/cache usage, content_block_delta streams text_delta chunks (the
// live estimate), and message_delta carries the message's real cumulative output_tokens (the
// authoritative per-message figure). The assembled type:"assistant" line still arrives, but under
// partials its usage.output_tokens is an early placeholder, so output is taken from message_delta —
// never the assistant line. The terminal type:"result" line is handled by classify/extractResultLine.
type streamLine struct {
	Type    string         `json:"type"`
	Message *streamMessage `json:"message"` // type:"assistant" (assembled message)
	Event   *streamEvent   `json:"event"`   // type:"stream_event": the wrapped SSE event
}

type streamEvent struct {
	Type    string         `json:"type"`    // message_start | content_block_delta | message_delta | …
	Message *streamMessage `json:"message"` // message_start: the opening message (input/cache usage)
	Delta   *streamDelta   `json:"delta"`   // content_block_delta: the text chunk
	Usage   *innerUsage    `json:"usage"`   // message_delta: the message's cumulative usage
}

type streamDelta struct {
	Type string `json:"type"` // "text_delta" for streamed output text
	Text string `json:"text"`
}

type streamMessage struct {
	Content []streamContent `json:"content"`
	Usage   *innerUsage     `json:"usage"`
}

type streamContent struct {
	Type  string          `json:"type"` // "tool_use" | "text" | … (only tool_use is read; live text streams via streamDelta)
	Name  string          `json:"name"` // tool_use: the tool name
	Input json.RawMessage `json:"input"`
}

// streamAccum carries a leaf's running token figure across its stream-json lines. ACCOUNTING is exact:
// doneOut sums each finalized message's REAL output (curOut, from its message_delta — cumulative, so the
// latest value wins and multiple deltas never double-count), falling back to a conservative runes
// estimate only for a message that streamed text but reported NO message_delta (some third-party
// Anthropic-compatible providers). DISPLAY is monotonic: maybeEmitUsage floors the emitted value at the
// max already shown (lastEmit), so a low real count landing after a higher estimate never dips the board
// — without inflating the accounting. Input is the peak (grows with context); cache the latest non-zero.
// The exact final still arrives from Result.Usage at completion.
type streamAccum struct {
	inTok       int  // latest input tokens (prompt seed, then real)
	cache       int  // latest cache-read input tokens
	doneOut     int  // exact summed output of finalized prior messages (real, or estimate when unmeasured)
	curOut      int  // current message's real cumulative output (0 until its message_delta)
	curMeasured bool // the current message reported a message_delta (its real output is known)
	turnRunes   int  // streamed text runes of the current message (estimate source when unmeasured)
	lastEmit    int  // max output already shown (display floor + throttle baseline)
}

// runesPerTokenEstimate converts streamed output runes into a CONSERVATIVE token estimate for an
// unmeasured in-flight message. It biases low (real output runs ≈4 chars/token, sometimes more) so the
// estimate usually sits under the real count and the figure settles UP to the exact total at completion.
const runesPerTokenEstimate = 5

// curContribution is the current message's output so far for the ACCOUNTING total: its real cumulative
// count once message_delta reported it, else the conservative runes estimate (so an unmeasured message
// still contributes). The real count wins for a measured message; the display floor (lastEmit) handles
// any visual dip separately, so the accounting is never inflated by an over-running estimate.
func (a *streamAccum) curContribution() int {
	if a.curMeasured {
		return a.curOut
	}
	return a.turnRunes / runesPerTokenEstimate
}

// estOut is the running output figure: finalized prior messages plus the current message's contribution.
func (a *streamAccum) estOut() int { return a.doneOut + a.curContribution() }

// foldInputCache lifts input/cache from a usage object (message_start, message_delta, or the
// assembled assistant line): input is the peak (it only grows with context), cache the latest
// non-zero. Output is never taken here — it comes only from message_delta.
func (a *streamAccum) foldInputCache(u *innerUsage) {
	if u == nil {
		return
	}
	if u.InputTokens > a.inTok {
		a.inTok = u.InputTokens
	}
	if u.CacheReadInputTokens > 0 {
		a.cache = u.CacheReadInputTokens
	}
}

// activityUsageStep throttles the climbing estimate: a content_block_delta emits a new usage row only
// once output has grown by at least this many tokens — bounding sidecar writes while still updating
// well within the board's 0.1k display resolution. A message_start / message_delta force-emits.
const activityUsageStep = 24

// maybeEmitUsage writes a usage snapshot on a forced event (message boundary / reconcile) or once the
// climbing estimate grew past the throttle. Nil-safe for the writer (scanLiveUsage accumulates only).
func maybeEmitUsage(w *activityWriter, a *streamAccum, force bool) {
	out := a.estOut()
	if out < a.lastEmit {
		out = a.lastEmit // display floor: never show less than already shown, even when a real count is lower
	}
	if a.inTok == 0 && out == 0 && a.cache == 0 {
		return
	}
	if !force && out-a.lastEmit < activityUsageStep {
		return
	}
	a.lastEmit = out
	w.emit(activityRecord{Kind: "usage", In: a.inTok, Out: out, Cache: a.cache})
}

// parseStreamLine decodes one stream-json line and updates the accumulator, emitting a tool record per
// tool_use block and throttled/reconciled usage snapshots. With --include-partial-messages the real
// per-message output arrives on message_delta as a cumulative count (the assistant line's usage is an
// early placeholder, so output is never taken from it); content_block_delta text feeds the in-flight
// estimate until then. A non-matching line / decode error is skipped; the terminal result line is
// handled by classify.
func parseStreamLine(line []byte, w *activityWriter, a *streamAccum) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var sl streamLine
	if json.Unmarshal(line, &sl) != nil {
		return
	}
	switch sl.Type {
	case "assistant":
		if sl.Message == nil {
			return
		}
		for _, c := range sl.Message.Content {
			if c.Type == "tool_use" && c.Name != "" {
				w.emit(activityRecord{Kind: "tool", Tool: c.Name, Arg: ToolArgPreview(c.Input)})
			}
		}
		a.foldInputCache(sl.Message.Usage) // input/cache only; output comes from message_delta
		maybeEmitUsage(w, a, false)
	case "stream_event":
		if sl.Event == nil {
			return
		}
		switch sl.Event.Type {
		case "message_start":
			// Finalize the prior message into the exact accounting total: its REAL count when it reported
			// a message_delta, else its runes estimate (an unmeasured provider's output isn't lost). The
			// display floor in maybeEmitUsage keeps the board from dipping; it never feeds back here, so a
			// measured message is never overcounted. A new message restarts at 0.
			a.doneOut += a.curContribution()
			a.curOut = 0
			a.curMeasured = false
			a.turnRunes = 0
			if sl.Event.Message != nil {
				a.foldInputCache(sl.Event.Message.Usage)
			}
			maybeEmitUsage(w, a, true) // establish the input/context figure right away (even before output)
		case "content_block_delta":
			if d := sl.Event.Delta; d != nil && d.Type == "text_delta" {
				a.turnRunes += utf8.RuneCountInString(d.Text)
				maybeEmitUsage(w, a, false)
			}
		case "message_delta":
			a.foldInputCache(sl.Event.Usage)
			if sl.Event.Usage != nil {
				a.curOut = sl.Event.Usage.OutputTokens // this message's cumulative output — latest wins, never summed
				a.curMeasured = true
			}
			maybeEmitUsage(w, a, true)
		}
	}
}

// scanState is a detached job's persisted scan checkpoint (<jobID>.scan): the byte offset consumed so
// far plus the running accumulator. Each StatusFor poll parses ONLY the .out bytes appended since the
// last one, so the per-poll cost is bounded to the new bytes and the running total is kept for the
// whole capture no matter how large it grows. The accounting (off + accumulator) is re-derivable, so a
// torn/missing checkpoint just restarts the scan from offset 0; but LastOut (the display floor) is NOT
// re-derivable, so scanLiveUsage serializes the whole read-modify-write per job (a flock on a dedicated
// <jobID>.scan.lock file) — concurrent board polls can't clobber the floor with a stale, lower value.
type scanState struct {
	Off       int64 `json:"off"`
	DoneOut   int   `json:"done_out"`
	CurOut    int   `json:"cur_out"`
	Measured  bool  `json:"measured"` // the in-flight message has reported a message_delta
	TurnRunes int   `json:"turn_runes"`
	InTok     int   `json:"in_tok"`
	Cache     int   `json:"cache"`
	LastOut   int   `json:"last_out"` // display floor: the max output already shown (never dips across polls)
}

func (st scanState) accum() *streamAccum {
	return &streamAccum{inTok: st.InTok, cache: st.Cache, doneOut: st.DoneOut, curOut: st.CurOut,
		curMeasured: st.Measured, turnRunes: st.TurnRunes, lastEmit: st.LastOut}
}

// usage is the figure shown to the board: the exact accounting total, floored at LastOut so it never
// dips across polls (a low real count never undoes a higher figure already shown). nil until anything
// has been reported.
func (st scanState) usage() *Usage {
	out := st.accum().estOut()
	if out < st.LastOut {
		out = st.LastOut
	}
	if st.InTok == 0 && out == 0 && st.Cache == 0 {
		return nil
	}
	return &Usage{InputTokens: st.InTok, OutputTokens: out, CacheReadInputTokens: st.Cache}
}

// scanLiveUsage updates a detached job's running usage by parsing only the stream-json .out bytes
// appended since the last poll (tracked by the <jobID>.scan checkpoint) and persisting the advanced
// checkpoint. The background lane has no live writer (its child is detached), so StatusFor calls this
// each poll to make the job's board token count climb without a sidecar. Returns nil when nothing has
// been reported yet. The checkpoint read-modify-write is serialized per job (a blocking flock keyed by
// the .scan path) so two concurrent board polls — the 500ms and 3s chains scan the same job — can't
// interleave: a slower poll can never write back a stale, lower display floor and tick the board down.
func scanLiveUsage(outPath, statePath string) *Usage {
	var u *Usage
	// Serialize on a DEDICATED lock file (a separate inode from the .scan data file the body reads and
	// rewrites): locking the data file itself and then re-opening it to write would collide on Windows,
	// where a byte-range lock plus a second write handle to the same file fails.
	if err := config.WithFlock(statePath+".lock", func() error {
		u = scanLiveUsageLocked(outPath, statePath)
		return nil
	}); err != nil {
		return nil // lock unobtainable (e.g. dir vanished) → no figure this poll, never a wrong one
	}
	return u
}

func scanLiveUsageLocked(outPath, statePath string) *Usage {
	st := loadScanState(statePath)
	f, err := os.Open(outPath)
	if err != nil {
		return st.usage()
	}
	defer f.Close()
	if fi, statErr := f.Stat(); statErr != nil || st.Off > fi.Size() {
		st = scanState{} // unreadable, or the capture is shorter than our offset (rotated) → restart
	}
	if _, err := f.Seek(st.Off, io.SeekStart); err != nil {
		return st.usage()
	}
	data, err := io.ReadAll(f) // only the new bytes since st.Off
	if err != nil {
		return st.usage()
	}
	a := st.accum()
	// Parse only COMPLETE lines (through the last newline); a partial trailing line still being written
	// is left for the next poll, so a delta is never half-counted.
	if nl := bytes.LastIndexByte(data, '\n'); nl >= 0 {
		for _, line := range bytes.Split(data[:nl], []byte{'\n'}) {
			parseStreamLine(line, nil, a) // nil writer: accumulate only, never emit
		}
		floor := a.estOut()
		if floor < st.LastOut {
			floor = st.LastOut // carry the display floor forward — the shown figure never dips
		}
		st = scanState{Off: st.Off + int64(nl) + 1, DoneOut: a.doneOut, CurOut: a.curOut, Measured: a.curMeasured,
			TurnRunes: a.turnRunes, InTok: a.inTok, Cache: a.cache, LastOut: floor}
		saveScanState(statePath, st)
	}
	return st.usage()
}

// loadScanState reads a job's scan checkpoint; a missing or torn file degrades to the zero state
// (a full re-scan), so a crash mid-write never corrupts the count.
func loadScanState(path string) scanState {
	var st scanState
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// saveScanState persists the advanced checkpoint, best-effort: the checkpoint is a regenerable cache
// (re-derivable from the .out), so a lost or torn write just costs the next poll a re-parse.
func saveScanState(path string, st scanState) {
	if data, err := json.Marshal(st); err == nil {
		_ = os.WriteFile(path, data, 0o600)
	}
}

// ToolArgPreview renders a tool_use input as a single safe line: the primary argument value
// (preferring a known primary key, else the first key in sorted order), key-masked and length-capped.
// Model-generated content — never the key (apiKeyHelper) — but masked + truncated defense-in-depth,
// then CleanTitle-scrubbed by the board at render. Exported because the board's teammate card
// projects transcript tool_use blocks into the same signature format the activity sidecar uses.
func ToolArgPreview(input json.RawMessage) string {
	if len(bytes.TrimSpace(input)) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(input, &obj) != nil || len(obj) == 0 {
		return clampArg(string(redact.MaskKeyLike(bytes.TrimSpace(input))))
	}
	var key string
	for _, primary := range []string{"command", "url", "query", "file_path", "path", "pattern", "prompt"} {
		if _, ok := obj[primary]; ok {
			key = primary
			break
		}
	}
	if key == "" {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		key = keys[0]
	}
	raw := obj[key]
	var s string
	if json.Unmarshal(raw, &s) != nil { // not a JSON string → use the compact JSON
		s = string(raw)
	}
	return clampArg(string(redact.MaskKeyLikeString(s)))
}

func clampArg(s string) string {
	if len(s) > maxActivityArg {
		return s[:maxActivityArg] + "…"
	}
	return s
}

// extractResultLine scans stream-json stdout for the single `type:"result"` envelope line and
// returns it for classify. The result line is NOT guaranteed to be last (a trailing SessionStart
// hook_response can follow it), so we scan rather than take the tail; the LAST result line wins if
// (defensively) more than one appears. An empty return makes classify fall back to SUBAGENT_FAILED.
func extractResultLine(stdout []byte) []byte {
	var out []byte
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 0, 64*1024), maxChildOutput)
	for sc.Scan() {
		line := sc.Bytes()
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &head) == nil && head.Type == "result" {
			out = append(out[:0], line...) // keep the latest result line
		}
	}
	return out
}
