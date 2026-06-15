package subagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestReadTailCapped_BoundsRead: the running-status scan must never read more than the cap, even when
// the file is far larger (the bound is what keeps the per-poll cost flat on a big background output).
func TestReadTailCapped_BoundsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.out")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 500)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readTailCapped(path, 100); len(got) != 100 {
		t.Fatalf("read %d bytes, want exactly the 100-byte cap", len(got))
	}
	// A file under the cap is returned whole.
	small := filepath.Join(t.TempDir(), "small.out")
	if err := os.WriteFile(small, []byte("hello"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readTailCapped(small, 100); string(got) != "hello" {
		t.Fatalf("small file should return whole, got %q", got)
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

// TestParseStreamLine_EstimateReconcilesToReal: the live figure is APPROXIMATE — an over-running
// estimate (400 runes / 5 → 80) yields to the authoritative per-message count (10) when message_delta
// lands. It tracks the truth rather than holding an inflated estimate; the exact final is Result.Usage.
func TestParseStreamLine_EstimateReconcilesToReal(t *testing.T) {
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
	if len(outs) != 2 || outs[0] != 80 || outs[1] != 10 {
		t.Fatalf("expected the estimate (80) then a reconcile to the real count (10), got %v", outs)
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

// TestScanLiveUsage_PartialAndReconciled: the background poll scan returns a climbing estimate from a
// partial capture (text deltas, no message_delta yet) and the reconciled real output once message_delta
// lands; a capture with no usage at all returns nil.
func TestScanLiveUsage_PartialAndReconciled(t *testing.T) {
	partial := []byte(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":2000}}}}` + "\n" +
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + strings.Repeat("x", 80) + `"}}}` + "\n")
	if u := scanLiveUsage(partial); u == nil || u.InputTokens != 2000 || u.OutputTokens != 16 {
		t.Fatalf("partial scan: want in=2000 out=16 (80 runes/5), got %+v", u)
	}
	full := append(append([]byte{}, partial...), []byte(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":42}}}`+"\n")...)
	if u := scanLiveUsage(full); u == nil || u.OutputTokens != 42 {
		t.Fatalf("full scan: want out=42 (reconciled), got %+v", u)
	}
	if scanLiveUsage([]byte(`{"type":"system"}`+"\n")) != nil {
		t.Fatalf("a capture with no usage should return nil")
	}
}
