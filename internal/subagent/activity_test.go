package subagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestFreshActivitySidecar_TruncatesStale: a restart reuses the job id, and the best-effort cleanup can
// leave a prior attempt's .activity (a failed remove). The new attempt must start from an EMPTY sidecar
// so the board, which stamps a snapshot's attempt from the live meta, never reads stale rows as the
// current attempt.
func TestFreshActivitySidecar_TruncatesStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.activity")
	if err := os.WriteFile(path, []byte(`{"kind":"usage","in":5000,"out":800}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed stale sidecar: %v", err)
	}
	freshActivitySidecar(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after fresh: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a new attempt must start from an empty sidecar, got %q", got)
	}
	// Truncate-only: an absent sidecar (the cleanup already removed it) is a no-op — never recreated as
	// an orphan empty file.
	absent := filepath.Join(t.TempDir(), "gone.activity")
	freshActivitySidecar(absent)
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatalf("freshActivitySidecar must not create an absent sidecar, got %v", err)
	}
}

// TestExtractResultLine: the type:"result" line is found by SCANNING (not last-line) — a trailing
// SessionStart hook_response after the result must not shadow it.
func TestExtractResultLine(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"the answer","total_cost_usd":0.01}`,
		`{"type":"system","subtype":"hook_response"}`, // trails the result line
	}, "\n") + "\n"
	line := extractResultLine([]byte(stream))
	var e innerEnvelope
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("extracted line not parseable: %v (%q)", err, line)
	}
	if e.Type != "result" || e.Result != "the answer" {
		t.Fatalf("extracted wrong line: %+v", e)
	}
	// A stream with no result line → empty (classify then falls back to SUBAGENT_FAILED).
	if got := extractResultLine([]byte(`{"type":"assistant"}` + "\n")); len(got) != 0 {
		t.Fatalf("no-result stream should yield empty, got %q", got)
	}
}

// TestExtractResultLine_StructuredOutputLift: a stream transcript whose terminal type:"result"
// line carries structured_output ends with the same lift as the plain json envelope —
// extractResultLine then classify, the exact sequence Run applies on the StreamActivity path.
func TestExtractResultLine_StructuredOutputLift(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"prose","structured_output":{"answer":5}}`,
	}, "\n") + "\n"
	req := Request{Provider: "v", StreamActivity: true}
	res := classify(req, "m", extractResultLine([]byte(stream)), nil, 0, false, true)
	if !res.OK || res.Result != "prose" {
		t.Fatalf("want OK + prose result, got OK=%v result=%q (%s)", res.OK, res.Result, res.ErrorCode)
	}
	assertJSONEq(t, res.StructuredOutput, `{"answer":5}`)
}

// TestExtractResultLine_OversizedLinePrecedingResult: an intermediate line larger than the per-line
// acceptance cap must not halt the scan — a successful run's terminal type:"result" line is still
// found and parsed.
func TestExtractResultLine_OversizedLinePrecedingResult(t *testing.T) {
	orig := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = orig })

	huge := `{"type":"user","message":{"content":[{"type":"tool_result","content":"` +
		strings.Repeat("x", maxChildOutput*2) + `"}]}}`
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		huge,
		`{"type":"result","subtype":"success","is_error":false,"result":"the answer","total_cost_usd":0.01}`,
		`{"type":"system","subtype":"hook_response"}`,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "job.out")
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	line := extractResultLineFile(path)
	var e innerEnvelope
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("extracted line not parseable: %v (%q)", err, line)
	}
	if e.Type != "result" || e.Result != "the answer" {
		t.Fatalf("oversized line halted the scan: %+v", e)
	}
}

// TestExtractResultLine_OversizedLineIsLast: an oversized line as the last physical line with no
// result anywhere yields empty — never a fabricated result (classify then fails honestly).
func TestExtractResultLine_OversizedLineIsLast(t *testing.T) {
	orig := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = orig })

	huge := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxChildOutput*2) + `"}`
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		huge,
	}, "\n") // no trailing newline: the oversized line is the final one
	path := filepath.Join(t.TempDir(), "job.out")
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if got := extractResultLineFile(path); len(got) != 0 {
		t.Fatalf("resultless capture must yield empty, got %q", got)
	}
}

// TestExtractResultLine_OversizedResultLineIsDiscarded: a type:"result" line that itself exceeds the
// per-line acceptance cap is discarded like any oversized line, so the extract yields empty — an
// over-cap result maps to no-result (classify then fails honestly), never a fabrication.
func TestExtractResultLine_OversizedResultLineIsDiscarded(t *testing.T) {
	orig := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = orig })

	hugeResult := `{"type":"result","subtype":"success","is_error":false,"result":"` +
		strings.Repeat("x", maxChildOutput*2) + `"}`
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		hugeResult,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "job.out")
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	if got := extractResultLineFile(path); len(got) != 0 {
		t.Fatalf("over-cap result line must be discarded (yield empty), got %q", got)
	}
}

// TestExtractResultLine_MemoryBounded: several oversized lines totaling many multiples of the cap
// still let the result line be found — the streaming reader never buffers a whole oversized body.
func TestExtractResultLine_MemoryBounded(t *testing.T) {
	orig := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = orig })

	big := strings.Repeat("x", maxChildOutput*3)
	stream := strings.Join([]string{
		`{"type":"user","message":{"content":"` + big + `"}`,
		`{"type":"user","message":{"content":"` + big + `"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"found it"}`,
		`{"type":"user","message":{"content":"` + big + `"}`,
	}, "\n") + "\n"
	path := filepath.Join(t.TempDir(), "job.out")
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	line := extractResultLineFile(path)
	var e innerEnvelope
	if err := json.Unmarshal(line, &e); err != nil {
		t.Fatalf("extracted line not parseable: %v (%q)", err, line)
	}
	if e.Type != "result" || e.Result != "found it" {
		t.Fatalf("result not found among oversized lines: %+v", e)
	}
}

// TestToolArgPreview: the primary arg value is extracted (known key first), key-masked, length-capped.
func TestToolArgPreview(t *testing.T) {
	cases := map[string]string{
		`{"command":"echo hi","timeout":5}`: "echo hi", // primary key "command" wins over "timeout"
		`{"url":"https://example.com"}`:     "https://example.com",
		`{"zeta":"z","alpha":"a"}`:          "a", // no known primary → first sorted key
	}
	for in, want := range cases {
		if got := ToolArgPreview(json.RawMessage(in)); got != want {
			t.Errorf("ToolArgPreview(%s) = %q, want %q", in, got, want)
		}
	}
	long := `{"command":"` + strings.Repeat("x", maxActivityArg+50) + `"}`
	if got := ToolArgPreview(json.RawMessage(long)); len(got) > maxActivityArg+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("a long arg should be capped with an ellipsis, got len %d", len(got))
	}
}

// TestActivitySink_CapFirstAndParses: the sink tees to the byte-cap FIRST (overflow still fires) and
// writes tool/usage rows parsed from the partial-message stream to the activity sidecar — input/cache
// from message_start, the tool from the assistant line, output reconciled from message_delta.
func TestActivitySink_CapFirstAndParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.activity")
	out := &cappedWriter{limit: 1 << 20}
	w := newActivityWriter(path)
	sink := newActivitySink(out, w)
	sink.Write([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3}}}}` + "\n"))
	sink.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}` + "\n"))
	sink.Write([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":42}}}` + "\n"))
	sink.Write([]byte(`{"type":"result","subtype":"success","result":"ok"}` + "\n"))
	sink.close()

	if out.buf.Len() == 0 {
		t.Fatal("sink must tee bytes into the cap")
	}
	recs := readActivity(t, path)
	var tool, usage *activityRecord
	for i := range recs {
		switch recs[i].Kind {
		case "tool":
			tool = &recs[i]
		case "usage":
			usage = &recs[i] // keep the latest usage row (what the board reads)
		}
	}
	if tool == nil || tool.Tool != "Bash" || tool.Arg != "echo hi" {
		t.Errorf("tool record = %+v", tool)
	}
	if usage == nil || usage.In != 10 || usage.Out != 42 || usage.Cache != 3 {
		t.Errorf("usage record = %+v", usage)
	}
	if len(recs) == 0 || recs[0].Seq != 1 {
		t.Errorf("seq must start at 1, got %+v", recs)
	}
}

// TestActivitySink_CapOverflowStillFires: wrapping the cap with the sink must not change the
// overflow→kill behaviour — the cap fires onOverflow on the write that exceeds the limit.
func TestActivitySink_CapOverflowStillFires(t *testing.T) {
	fired := false
	out := &cappedWriter{limit: 8, onOverflow: func() { fired = true }}
	sink := newActivitySink(out, newActivityWriter(filepath.Join(t.TempDir(), "y.activity")))
	n, err := sink.Write([]byte("0123456789ABCDEF")) // 16 > 8
	sink.close()
	if n != 16 || err != nil {
		t.Fatalf("sink.Write should report (len(p), nil) like the cap, got (%d, %v)", n, err)
	}
	if !fired {
		t.Fatal("overflow must still fire through the sink (cap-first)")
	}
}

// TestFinalizeSyncJob_KeepsSafeMetricsStripsAnswer: the sanitized sync cache keeps Usage/cost/turns
// (the board needs them) but never the answer text.
func TestFinalizeSyncJob_KeepsSafeMetricsStripsAnswer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	jobID := regSyncJob(Request{Provider: "glm", PersistIO: true}, "glm-4.6")
	if jobID == "" {
		t.Fatal("registerSyncJob returned empty id")
	}
	finalizeSyncJob(jobID, Result{
		OK: true, Result: "PLANTED_ANSWER_CANARY",
		Usage:   &Usage{InputTokens: 1200, OutputTokens: 340, CacheReadInputTokens: 50},
		CostUSD: 0.0123, NumTurns: 4, DurationMs: 5000, StopReason: "end_turn",
	})
	got := StatusFor(jobID)
	if got.Status != "done" {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.Usage == nil || got.Usage.InputTokens != 1200 || got.CostUSD != 0.0123 || got.NumTurns != 4 {
		t.Errorf("safe metrics not persisted: usage=%+v cost=%v turns=%d", got.Usage, got.CostUSD, got.NumTurns)
	}
	if got.Result != "" {
		t.Errorf("the answer must be stripped from the cache, got %q", got.Result)
	}
}

func readActivity(t *testing.T, path string) []activityRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open activity: %v", err)
	}
	defer f.Close()
	var out []activityRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r activityRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// TestParseStreamLine_EstimatesThenReconciles: a content_block_delta streams text → a live runes-based
// estimate climbs (so the board moves mid-turn); message_delta then reconciles output to the message's
// real figure (replacing the estimate, not adding to it).
func TestParseStreamLine_EstimatesThenReconciles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.activity")
	w := newActivityWriter(path)
	a := &streamAccum{}
	// 150 runes / 5 → 30 estimate (past the throttle), emitted live.
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("字", 150)+`"}}}`), w, a)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"out":30`) {
		t.Fatalf("expected a live output estimate (~30) from streamed text, got:\n%s", data)
	}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":99}}}`), w, a)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"out":99`) {
		t.Fatalf("message_delta should reconcile output to the real 99, got:\n%s", data)
	}
}

// TestParseStreamLine_CrossMessageOutput: per-message real outputs accumulate ACROSS messages (a new
// message_start finalizes the prior message's count), so a multi-turn leaf's total climbs 50 → 120.
func TestParseStreamLine_CrossMessageOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.activity")
	w := newActivityWriter(path)
	a := &streamAccum{}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":50}}}`), w, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{}}}}`), w, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":70}}}`), w, a)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"out":50`) || !strings.Contains(string(data), `"out":120`) {
		t.Fatalf("expected cross-message rows 50 then 120 (50 + the second message's 70), got:\n%s", data)
	}
}

// TestParseStreamLine_CumulativeDeltaNotSummed: multiple message_delta events WITHIN one message carry
// a growing CUMULATIVE output; the latest wins (30 then 40 → 40), never summed (would be 70).
func TestParseStreamLine_CumulativeDeltaNotSummed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.activity")
	w := newActivityWriter(path)
	a := &streamAccum{}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":30}}}`), w, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":40}}}`), w, a)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"out":40`) {
		t.Fatalf("the latest cumulative output (40) must win, got:\n%s", data)
	}
	if strings.Contains(string(data), `"out":70`) {
		t.Fatalf("two cumulative deltas in one message must NOT be summed to 70, got:\n%s", data)
	}
}

// TestParseStreamLine_MonotonicHoldsEstimate: the live figure never dips — an over-running estimate
// (400 runes / 5 → 80) is HELD when a lower real count (10) lands, because the current message
// contributes max(real, estimate). The exact final still arrives from Result.Usage at completion.
func TestParseStreamLine_MonotonicHoldsEstimate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.activity")
	w := newActivityWriter(path)
	a := &streamAccum{}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("x", 400)+`"}}}`), w, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":10}}}`), w, a)
	var outs []int
	for _, r := range readActivity(t, path) {
		if r.Kind == "usage" {
			outs = append(outs, r.Out)
		}
	}
	if len(outs) == 0 {
		t.Fatal("expected at least one usage row")
	}
	for i, o := range outs {
		if o < 80 {
			t.Fatalf("the figure must not dip below the estimate (80); outs=%v (row %d = %d)", outs, i, o)
		}
	}
}

// TestParseStreamLine_MeasuredCarriesReal: when an over-running estimate (400 runes → 80) is followed by
// a LOWER real count (message_delta 10), the ACCOUNTING carries the real 10 into doneOut at the next
// message_start — never the inflated estimate — while the DISPLAY floor holds the figure (≥80) so the
// board doesn't dip. This keeps a measured message from being permanently overcounted.
func TestParseStreamLine_MeasuredCarriesReal(t *testing.T) {
	a := &streamAccum{}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("x", 400)+`"}}}`), nil, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":10}}}`), nil, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{}}}}`), nil, a)
	if a.doneOut != 10 {
		t.Fatalf("a measured message must carry its REAL count (10) into doneOut, not the estimate (80); doneOut=%d", a.doneOut)
	}
	if a.lastEmit < 80 {
		t.Fatalf("the display floor must hold the figure (≥80), got lastEmit=%d", a.lastEmit)
	}
}

// TestParseStreamLine_NoDeltaCarriesEstimate: a message that streamed text but reported NO message_delta
// (some third-party Anthropic-compatible providers) must not lose its output — at the next message_start
// its estimate is carried into doneOut, and the running total stays ≥ the first message's estimate.
func TestParseStreamLine_NoDeltaCarriesEstimate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nd.activity")
	w := newActivityWriter(path)
	a := &streamAccum{}
	// msg1: 500 runes / 5 → 100 estimate, NO message_delta.
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("x", 500)+`"}}}`), w, a)
	// msg2 begins: msg1's 100 estimate must carry into doneOut, not vanish.
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{}}}}`), w, a)
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("y", 150)+`"}}}`), w, a)
	var last int
	for _, r := range readActivity(t, path) {
		if r.Kind == "usage" {
			last = r.Out
		}
	}
	if last < 130 { // carried 100 + the 30 of msg2's in-flight estimate
		t.Fatalf("a no-message_delta message's estimate must carry across the boundary, got last out=%d", last)
	}
}

// TestParseStreamLine_InputSeedCarry: input is seeded (the prompt estimate); message_start force-emits
// it right away so the board's context figure shows before any output, and a real, larger
// usage.input_tokens then supersedes the seed.
func TestParseStreamLine_InputSeedCarry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.activity")
	w := newActivityWriter(path)
	a := &streamAccum{inTok: 1200} // input seeded from the prompt estimate
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{}}}}`), w, a)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"in":1200`) {
		t.Fatalf("message_start must emit the seeded input (1200), got:\n%s", data)
	}
	parseStreamLine([]byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":5000,"output_tokens":10}}}`), w, a)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"in":5000`) {
		t.Fatalf("a real input_tokens (5000) must supersede the seed, got:\n%s", data)
	}
}

// TestScanLiveUsage_IncrementalCheckpoint: the background scan parses only the bytes appended since the
// last poll (tracked by the .scan checkpoint) and keeps the running total across the whole capture, so
// the count climbs across polls and never drops regardless of file size.
func TestScanLiveUsage_IncrementalCheckpoint(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "j.out")
	statePath := filepath.Join(dir, "j.scan")
	write := func(s string) {
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}

	// Poll 1: message_start + a text delta → the live estimate (80 runes / 5 = 16).
	write(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":2000}}}}` + "\n")
	write(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + strings.Repeat("x", 80) + `"}}}` + "\n")
	u1 := scanLiveUsage(outPath, statePath)
	if u1 == nil || u1.InputTokens != 2000 || u1.OutputTokens != 16 {
		t.Fatalf("poll 1: want in=2000 out=16, got %+v", u1)
	}
	off1 := loadScanState(statePath).Off
	if off1 == 0 {
		t.Fatal("checkpoint offset should advance after poll 1")
	}

	// Poll 2: append msg1's real count and a whole second message → totals accumulate across polls.
	write(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":50}}}` + "\n")
	write(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{}}}}` + "\n")
	write(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":70}}}` + "\n")
	u2 := scanLiveUsage(outPath, statePath)
	if u2 == nil || u2.OutputTokens != 120 { // 50 (msg1) carried + 70 (msg2)
		t.Fatalf("poll 2: want out=120 (50+70 accumulated across the capture), got %+v", u2)
	}
	if u2.OutputTokens < u1.OutputTokens {
		t.Fatalf("the running figure dropped across polls: %d -> %d", u1.OutputTokens, u2.OutputTokens)
	}
	if loadScanState(statePath).Off <= off1 {
		t.Fatal("checkpoint offset should advance again on poll 2")
	}

	// An absent capture (nothing reported yet) returns nil.
	if scanLiveUsage(filepath.Join(dir, "absent.out"), filepath.Join(dir, "absent.scan")) != nil {
		t.Fatal("an absent capture should return nil")
	}
}

// TestScanLiveUsage_FloorHeldWhenRealLower: a later poll whose accounting drops (a real per-message
// count landing under an earlier estimate) must not lower the persisted display floor — the board
// figure holds, while the accounting underneath stays exact.
func TestScanLiveUsage_FloorHeldWhenRealLower(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "j.out")
	statePath := filepath.Join(dir, "j.scan")
	write := func(s string) {
		f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	// Poll 1: 500 runes, no message_delta → estimate 100 → floor 100.
	write(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + strings.Repeat("x", 500) + `"}}}` + "\n")
	if u := scanLiveUsage(outPath, statePath); u == nil || u.OutputTokens != 100 {
		t.Fatalf("poll 1: want out=100, got %+v", u)
	}
	// Poll 2: the real per-message count lands low (10); the display floor holds 100.
	write(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":10}}}` + "\n")
	if u := scanLiveUsage(outPath, statePath); u == nil || u.OutputTokens != 100 {
		t.Fatalf("poll 2: the floor must hold at 100, got %+v", u)
	}
	if got := loadScanState(statePath).LastOut; got != 100 {
		t.Fatalf("persisted floor must stay 100, got %d", got)
	}
}

// TestScanLiveUsage_ConcurrentPollsHoldFloor: the 500ms and 3s board chains scan the same job
// concurrently; the per-job flock serializes the checkpoint read-modify-write, so no poll sees the
// floor dip and the file never races (run under -race).
func TestScanLiveUsage_ConcurrentPollsHoldFloor(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "j.out")
	statePath := filepath.Join(dir, "j.scan")
	if err := os.WriteFile(outPath, []byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"`+strings.Repeat("x", 500)+`"}}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	scanLiveUsage(outPath, statePath) // establish the floor at 100
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u := scanLiveUsage(outPath, statePath); u == nil || u.OutputTokens < 100 {
				t.Errorf("a concurrent poll saw the floor dip: %+v", u)
			}
		}()
	}
	wg.Wait()
	if got := loadScanState(statePath).LastOut; got != 100 {
		t.Fatalf("persisted floor changed under concurrent polls: %d", got)
	}
}
