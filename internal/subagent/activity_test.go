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
// writes tool/usage rows parsed from the stream to the activity sidecar.
func TestActivitySink_CapFirstAndParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.activity")
	out := &cappedWriter{limit: 1 << 20}
	w := newActivityWriter(path)
	sink := newActivitySink(out, w)
	sink.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3}}}` + "\n"))
	sink.Write([]byte(`{"type":"result","subtype":"success","result":"ok"}` + "\n"))
	sink.close()

	if out.buf.Len() == 0 {
		t.Fatal("sink must tee bytes into the cap")
	}
	recs := readActivity(t, path)
	if len(recs) != 2 {
		t.Fatalf("got %d activity records, want 2 (tool + usage); %+v", len(recs), recs)
	}
	if recs[0].Kind != "tool" || recs[0].Tool != "Bash" || recs[0].Arg != "echo hi" {
		t.Errorf("tool record = %+v", recs[0])
	}
	if recs[1].Kind != "usage" || recs[1].In != 10 || recs[1].Out != 2 || recs[1].Cache != 3 {
		t.Errorf("usage record = %+v", recs[1])
	}
	if recs[0].Seq != 1 || recs[1].Seq != 2 {
		t.Errorf("seqs not monotonic: %d, %d", recs[0].Seq, recs[1].Seq)
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

// TestParseStreamLine_EstimatesOutput: when an assistant message streams text but NO usage (the
// provider case), parseStreamLine emits a live OUTPUT estimate (~runes/3) so the board count climbs.
func TestParseStreamLine_EstimatesOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.activity")
	w := newActivityWriter(path)
	outRunes, outTokens, inTokens := 0, 0, 0
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("字", 30) + `"}]}}`)
	parseStreamLine(line, w, &outRunes, &outTokens, &inTokens)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"out":10`) { // 30 runes / 3, no real usage yet
		t.Fatalf("expected an estimated output usage row (~10), got:\n%s", data)
	}
	// A measured message adds its real output (99) on top of the carried estimate of the earlier
	// unmeasured message (10) → 109; the measured message's own text is not double-counted as estimate.
	line2 := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":5,"output_tokens":99}}}`)
	parseStreamLine(line2, w, &outRunes, &outTokens, &inTokens)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"out":109`) {
		t.Fatalf("the measured output (99) should add to the carried estimate (10) → 109, got:\n%s", data)
	}
}

// TestParseStreamLine_CumulativeOutput: successive real per-message output_tokens accumulate into a
// monotonic leaf total (50 then 120) — never the later single message's 70 — so the count climbs and
// never steps backward.
func TestParseStreamLine_CumulativeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.activity")
	w := newActivityWriter(path)
	outRunes, outTokens, inTokens := 0, 0, 0
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],"usage":{"output_tokens":50}}}`), w, &outRunes, &outTokens, &inTokens)
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"b"}],"usage":{"output_tokens":70}}}`), w, &outRunes, &outTokens, &inTokens)
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"out":50`) || !strings.Contains(string(data), `"out":120`) {
		t.Fatalf("expected cumulative output rows 50 then 120, got:\n%s", data)
	}
	if strings.Contains(string(data), `"out":70`) {
		t.Fatalf("the later message's raw 70 must not be emitted (it should sum to 120), got:\n%s", data)
	}
}

// TestParseStreamLine_NoBackwardWhenRealFollowsEstimate: an unmeasured message's estimate (300 runes →
// 100) is CARRIED, not undercut, when a later measured message lands — its real output (10) ADDS to the
// carried estimate (→ 110), so the count climbs and never steps backward across the estimate→real edge.
func TestParseStreamLine_NoBackwardWhenRealFollowsEstimate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.activity")
	w := newActivityWriter(path)
	outRunes, outTokens, inTokens := 0, 0, 0
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"`+strings.Repeat("x", 300)+`"}]}}`), w, &outRunes, &outTokens, &inTokens)
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"y"}],"usage":{"output_tokens":10}}}`), w, &outRunes, &outTokens, &inTokens)
	var outs []int
	for _, r := range readActivity(t, path) {
		if r.Kind == "usage" {
			outs = append(outs, r.Out)
		}
	}
	if len(outs) != 2 || outs[0] != 100 || outs[1] != 110 {
		t.Fatalf("expected the estimate (100) then real-added-to-carried-estimate (110), got %v", outs)
	}
}

// TestParseStreamLine_InputSeedCarry: input is seeded (the prompt estimate) and carried, so a tool-only
// assistant message (no usage, no text) still emits a usage row with the seeded input — the board's
// live token count is never a literal 0; a real, larger usage.input_tokens then supersedes the seed.
func TestParseStreamLine_InputSeedCarry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.activity")
	w := newActivityWriter(path)
	outRunes, outTokens, inTokens := 0, 0, 1200 // input seeded from the prompt estimate
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`), w, &outRunes, &outTokens, &inTokens)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"in":1200`) {
		t.Fatalf("a tool-only message must still emit the seeded input (1200), got:\n%s", data)
	}
	parseStreamLine([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":5000,"output_tokens":10}}}`), w, &outRunes, &outTokens, &inTokens)
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"in":5000`) {
		t.Fatalf("a real input_tokens (5000) must supersede the seed, got:\n%s", data)
	}
}
