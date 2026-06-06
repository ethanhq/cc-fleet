package subagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/ethanhq/cc-fleet/internal/redact"
)

// activityRecord is one line of a leaf's activity sidecar (<jobID>.activity, NDJSON, 0600). It is
// PURE OBSERVABILITY + content-privacy (PersistIO-gated, like <id>.prompt/.answer): a "tool" record
// carries the tool name + a masked/truncated preview of the model's first tool input; a "usage"
// record carries a running token snapshot. It NEVER carries the vendor key (that flows only through
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
	// even for a vendor that reports no per-message usage; a real usage.input_tokens supersedes it.
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

// estimatePromptTokens roughly estimates a prompt's input-token count from its rune count (≈3 runes
// per token), used to seed the live input figure before the vendor reports real usage.
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

// consume reassembles complete NDJSON lines across chunk boundaries and parses each. outRunes and
// outTokens accumulate, across the whole run, the model's streamed output text (for the pre-usage
// estimate) and the summed real per-message output tokens (the leaf's running output total).
func (s *activitySink) consume(w *activityWriter) {
	defer close(s.done)
	var buf []byte
	var outRunes, outTokens int
	inTokens := 0
	if w != nil {
		inTokens = w.inputSeed // start input at the prompt estimate; a real usage.input_tokens supersedes it
	}
	for chunk := range s.lines {
		buf = append(buf, chunk...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			parseStreamLine(buf[:i], w, &outRunes, &outTokens, &inTokens)
			buf = buf[i+1:]
		}
	}
}

// streamLine / streamMessage / streamContent are the LENIENT view of a `--output-format stream-json`
// NDJSON line — only the fields activity capture needs; everything else is ignored.
type streamLine struct {
	Type    string         `json:"type"`
	Message *streamMessage `json:"message"`
}

type streamMessage struct {
	Content []streamContent `json:"content"`
	Usage   *innerUsage     `json:"usage"`
}

type streamContent struct {
	Type  string          `json:"type"` // "text" | "tool_use" | "tool_result" | …
	Name  string          `json:"name"` // tool_use: the tool name
	Input json.RawMessage `json:"input"`
	Text  string          `json:"text"` // text block: the streamed model output (for the live token estimate)
}

// parseStreamLine decodes one stream-json line: it emits a tool record per tool_use block and one
// usage snapshot per assistant message. Output is a MONOTONIC cumulative for the leaf: the summed real
// output_tokens of MEASURED messages (those whose usage the vendor reported) PLUS a runes/3 estimate
// for the text of UNMEASURED messages (≈3 runes/token). A measured message's own text does NOT feed
// the estimate — its real count already covers it — so confirmed usage is never overridden by the
// estimate, and the two non-decreasing terms keep the count from ever stepping backward. cc-fleet
// parses CLAUDE's normalized stream (`--output-format stream-json --verbose`, no partial messages →
// exactly one usage per assistant message), so summing the measured messages is the cumulative without
// double-counting; the estimate covers vendors that stream no per-message usage until the result line.
// Input is seeded from the prompt estimate (so the live count is never 0 even for a no-usage,
// tool-heavy leaf) and superseded by a real, larger usage.input_tokens; cache is the latest reported.
// The accurate final still arrives from Result.Usage on completion. A non-assistant line / decode
// error is skipped; the result line is handled by classify.
func parseStreamLine(line []byte, w *activityWriter, outRunes, outTokens, inTokens *int) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var sl streamLine
	if json.Unmarshal(line, &sl) != nil || sl.Type != "assistant" || sl.Message == nil {
		return
	}
	msgRunes := 0
	for _, c := range sl.Message.Content {
		if c.Type == "tool_use" && c.Name != "" {
			w.emit(activityRecord{Kind: "tool", Tool: c.Name, Arg: toolArgPreview(c.Input)})
		}
		if c.Type == "text" && c.Text != "" {
			msgRunes += utf8.RuneCountInString(c.Text)
		}
	}
	cache := 0
	if u := sl.Message.Usage; u != nil {
		cache = u.CacheReadInputTokens
		if u.InputTokens > *inTokens {
			*inTokens = u.InputTokens // real per-turn input (the growing context) supersedes the seed
		}
		*outTokens += u.OutputTokens // measured message: its real output joins the running total
	} else {
		*outRunes += msgRunes // unmeasured message: its text feeds the estimate
	}
	in := *inTokens
	out := *outTokens + *outRunes/3 // real (measured) + estimate (unmeasured) — both non-decreasing
	if in > 0 || out > 0 || cache > 0 {
		w.emit(activityRecord{Kind: "usage", In: in, Out: out, Cache: cache})
	}
}

// toolArgPreview renders a tool_use input as a single safe line: the primary argument value
// (preferring a known primary key, else the first key in sorted order), key-masked and length-capped.
// Model-generated content — never the key (apiKeyHelper) — but masked + truncated defense-in-depth,
// then CleanTitle-scrubbed by the board at render.
func toolArgPreview(input json.RawMessage) string {
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
