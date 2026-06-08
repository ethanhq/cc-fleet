package codexproxy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture replay: live-captured (sanitized) Responses SSE streams from the
// ChatGPT codex backend, run through the converter and held to the full
// Anthropic stream grammar. These pin the converter against the real wire
// shape, not a hand-written approximation.

func replayFixture(t *testing.T, name string) *recSink {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".sse"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	c := newStreamConverter(sink, "gpt-5.5")
	if err := c.Convert(bytes.NewReader(b)); err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return sink
}

// assertGrammar checks the Anthropic stream invariants: one message_start
// first, every opened block closed before message_delta, one message_delta,
// message_stop last, deltas only on open blocks.
func assertGrammar(t *testing.T, sink *recSink) {
	t.Helper()
	if len(sink.events) == 0 || sink.events[0] != "message_start" {
		t.Fatalf("stream must open with message_start: %s", sink.seq())
	}
	if sink.events[len(sink.events)-1] != "message_stop" {
		t.Fatalf("stream must end with message_stop: %s", sink.seq())
	}
	open := map[any]bool{}
	deltaSeen := false
	for i, ev := range sink.events {
		p, _ := sink.payloads[i].(map[string]any)
		switch ev {
		case "content_block_start":
			idx := p["index"]
			if open[idx] {
				t.Fatalf("block %v opened twice", idx)
			}
			open[idx] = true
		case "content_block_delta":
			if !open[p["index"]] {
				t.Fatalf("delta on closed/unopened block %v", p["index"])
			}
		case "content_block_stop":
			if !open[p["index"]] {
				t.Fatalf("stop on unopened block %v", p["index"])
			}
			delete(open, p["index"])
		case "message_delta":
			if len(open) != 0 {
				t.Fatalf("message_delta with %d block(s) still open", len(open))
			}
			deltaSeen = true
		}
	}
	if !deltaSeen {
		t.Fatal("no message_delta emitted")
	}
}

// collectDeltas concatenates every delta value of the given delta type/field.
func collectDeltas(sink *recSink, deltaType, field string) string {
	var sb strings.Builder
	for i, ev := range sink.events {
		if ev != "content_block_delta" {
			continue
		}
		p, _ := sink.payloads[i].(map[string]any)
		d, _ := p["delta"].(map[string]any)
		if d["type"] == deltaType {
			s, _ := d[field].(string)
			sb.WriteString(s)
		}
	}
	return sb.String()
}

func stopReason(t *testing.T, sink *recSink) string {
	t.Helper()
	p := sink.payload("message_delta", 0)
	d, _ := p["delta"].(map[string]any)
	s, _ := d["stop_reason"].(string)
	return s
}

func TestFixture_Text(t *testing.T) {
	sink := replayFixture(t, "text")
	assertGrammar(t, sink)
	if got := collectDeltas(sink, "text_delta", "text"); got != "pong" {
		t.Fatalf("text = %q", got)
	}
	if sr := stopReason(t, sink); sr != "end_turn" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

func TestFixture_ForcedTool(t *testing.T) {
	sink := replayFixture(t, "forced_tool")
	assertGrammar(t, sink)
	// The tool_use block carries the captured call id + name.
	var toolBlock map[string]any
	for i, ev := range sink.events {
		if ev != "content_block_start" {
			continue
		}
		p, _ := sink.payloads[i].(map[string]any)
		cb, _ := p["content_block"].(map[string]any)
		if cb["type"] == "tool_use" {
			toolBlock = cb
		}
	}
	if toolBlock == nil || toolBlock["name"] != "lookup" {
		t.Fatalf("tool_use block = %v", toolBlock)
	}
	args := collectDeltas(sink, "input_json_delta", "partial_json")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil || parsed["key"] != "alpha" {
		t.Fatalf("streamed tool args %q: %v", args, err)
	}
	if sr := stopReason(t, sink); sr != "tool_use" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

func TestFixture_Thinking(t *testing.T) {
	sink := replayFixture(t, "thinking")
	assertGrammar(t, sink)
	if got := collectDeltas(sink, "signature_delta", "signature"); !strings.Contains(got, "gAAAAA-fixture-stub") {
		t.Fatalf("reasoning encrypted_content not surfaced as a signature: %q", got)
	}
	if got := collectDeltas(sink, "text_delta", "text"); got != "391" {
		t.Fatalf("answer text = %q", got)
	}
}
