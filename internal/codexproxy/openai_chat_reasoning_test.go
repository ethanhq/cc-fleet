package codexproxy

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// blockTypes lists the content_block_start types in stream order.
func blockTypes(sink *recSink) []string {
	var out []string
	for i, ev := range sink.events {
		if ev != "content_block_start" {
			continue
		}
		p, _ := sink.payloads[i].(map[string]any)
		cb, _ := p["content_block"].(map[string]any)
		s, _ := cb["type"].(string)
		out = append(out, s)
	}
	return out
}

// maxOpenBlocks reports the most blocks open at once, which must stay 1 for a
// thinking/text sequence: Anthropic content blocks never interleave.
func maxOpenBlocks(sink *recSink) int {
	open, most := 0, 0
	for _, ev := range sink.events {
		switch ev {
		case "content_block_start":
			open++
			if open > most {
				most = open
			}
		case "content_block_stop":
			open--
		}
	}
	return most
}

func TestChatFixture_ReasoningStreamsAsThinking(t *testing.T) {
	sink := replayChat(t, "chat_reasoning")
	assertGrammar(t, sink)
	if got := collectDeltas(sink, "thinking_delta", "thinking"); got != "The user wants a ping." {
		t.Fatalf("thinking = %q", got)
	}
	if got := collectDeltas(sink, "text_delta", "text"); got != "pong" {
		t.Fatalf("text = %q (reasoning must not leak into text)", got)
	}
	if got := blockTypes(sink); len(got) != 2 || got[0] != "thinking" || got[1] != "text" {
		t.Fatalf("blocks = %v, want [thinking text]", got)
	}
	if n := maxOpenBlocks(sink); n != 1 {
		t.Fatalf("thinking must close before text opens; %d blocks open at once", n)
	}
	if sr := stopReason(t, sink); sr != "end_turn" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

func TestChatFixture_ReasoningThenToolCall(t *testing.T) {
	sink := replayChat(t, "chat_reasoning_tool")
	assertGrammar(t, sink)
	if got := blockTypes(sink); len(got) != 2 || got[0] != "thinking" || got[1] != "tool_use" {
		t.Fatalf("blocks = %v, want [thinking tool_use]", got)
	}
	if n := maxOpenBlocks(sink); n != 1 {
		t.Fatalf("thinking must close before the tool block opens; %d open at once", n)
	}
	if got := collectDeltas(sink, "input_json_delta", "partial_json"); got != `{"key":"alpha"}` {
		t.Fatalf("tool args = %q", got)
	}
	if sr := stopReason(t, sink); sr != "tool_use" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

func TestChatFixture_ReasoningAliasAndResume(t *testing.T) {
	sink := replayChat(t, "chat_reasoning_resumed")
	assertGrammar(t, sink)
	want := []string{"thinking", "text", "thinking", "text"}
	got := blockTypes(sink)
	if len(got) != len(want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
	}
	if n := maxOpenBlocks(sink); n != 1 {
		t.Fatalf("blocks interleaved; %d open at once", n)
	}
	if got := collectDeltas(sink, "thinking_delta", "thinking"); got != "first thinksecond think" {
		t.Fatalf("thinking = %q", got)
	}
	if got := collectDeltas(sink, "text_delta", "text"); got != "partial answer" {
		t.Fatalf("text = %q", got)
	}
}

func TestChatTranslate_ThinkingBecomesReasoningContent(t *testing.T) {
	content := `[{"type":"thinking","thinking":"Need to ","signature":""},{"type":"thinking","thinking":"look it up.","signature":""},{"type":"tool_use","id":"t1","name":"lookup","input":{"k":"v"}}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":`+content+`}]}`)
	r, err := translateChatRequest(a, newConvCtx(a, ""))
	if err != nil {
		t.Fatal(err)
	}
	m := asMap(t, r.Messages[0])
	if m["reasoning_content"] != "Need to look it up." {
		t.Fatalf("reasoning_content = %#v", m["reasoning_content"])
	}
	if _, ok := m["tool_calls"]; !ok {
		t.Fatalf("tool_calls missing: %v", m)
	}
}

func TestChatTranslate_NoReasoningContentWithoutThinking(t *testing.T) {
	content := `[{"type":"text","text":"let me check"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":`+content+`}]}`)
	r, _ := translateChatRequest(a, newConvCtx(a, ""))
	if _, ok := asMap(t, r.Messages[0])["reasoning_content"]; ok {
		t.Fatal("reasoning_content must be absent when the turn had no thinking")
	}
}

func TestChatTranslate_OnlyUnsignedThinkingReplays(t *testing.T) {
	content := `[{"type":"thinking","thinking":"from claude","signature":"EqQBsig"},{"type":"thinking","thinking":"from chat","signature":""},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":`+content+`}]}`)
	r, err := translateChatRequest(a, newConvCtx(a, ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := asMap(t, r.Messages[0])["reasoning_content"]; got != "from chat" {
		t.Fatalf("reasoning_content = %#v, want only the unsigned block", got)
	}
}

func TestChatTranslate_SignedThinkingSendsNoReasoningContent(t *testing.T) {
	content := `[{"type":"thinking","thinking":"from claude","signature":"EqQBsig"},{"type":"tool_use","id":"t1","name":"lookup","input":{}}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":`+content+`}]}`)
	r, err := translateChatRequest(a, newConvCtx(a, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := asMap(t, r.Messages[0])["reasoning_content"]; ok {
		t.Fatal("a signed thinking block must not be replayed as reasoning_content")
	}
}

// A reasoning value that is not a string must not cost the chunk its content, tool
// calls, finish_reason or usage.
func TestChatStream_NonStringReasoningIgnored(t *testing.T) {
	for _, reasoning := range []string{`{}`, `[]`, `null`} {
		t.Run(reasoning, func(t *testing.T) {
			// Everything rides the one chunk that carries the odd reasoning value.
			sse := `data: {"choices":[{"delta":{"content":"pong","reasoning":` + reasoning + `,` +
				`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"key\":\"alpha\"}"}}]},` +
				`"finish_reason":"length"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}` + "\n\n" +
				`data: [DONE]` + "\n\n"
			sink := &recSink{}
			if err := newChatStreamConverter(sink, ccTest("m")).Convert(strings.NewReader(sse)); err != nil {
				t.Fatal(err)
			}
			assertGrammar(t, sink)
			if got := collectDeltas(sink, "text_delta", "text"); got != "pong" {
				t.Fatalf("text = %q, the chunk was dropped", got)
			}
			if got := collectDeltas(sink, "input_json_delta", "partial_json"); got != `{"key":"alpha"}` {
				t.Fatalf("tool arguments = %q, the chunk was dropped", got)
			}
			if got := strings.Join(blockTypes(sink), ","); got != "text,tool_use" {
				t.Fatalf("blocks = %s, want text,tool_use (no thinking block)", got)
			}
			// Without the chunk's finish_reason the open tool block would make this tool_use.
			if sr := stopReason(t, sink); sr != "max_tokens" {
				t.Fatalf("stop_reason = %q", sr)
			}
			usage, _ := sink.payload("message_delta", 0)["usage"].(map[string]any)
			if usage["input_tokens"] != 12 || usage["output_tokens"] != 3 {
				t.Fatalf("usage = %v, the chunk was dropped", usage)
			}
		})
	}
}

func TestChatFixture_ReasoningCutByLength(t *testing.T) {
	sink := replayChat(t, "chat_reasoning_length")
	assertGrammar(t, sink)
	if got := blockTypes(sink); len(got) != 1 || got[0] != "thinking" {
		t.Fatalf("blocks = %v, want [thinking]", got)
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := sink.seq(); got != want {
		t.Fatalf("events = %s\nwant     %s", got, want)
	}
	if sr := stopReason(t, sink); sr != "max_tokens" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

// blockEvents lists block starts and stops in stream order, e.g. "start tool_use@0".
func blockEvents(sink *recSink) []string {
	var out []string
	for i, ev := range sink.events {
		p, _ := sink.payloads[i].(map[string]any)
		switch ev {
		case "content_block_start":
			cb, _ := p["content_block"].(map[string]any)
			out = append(out, fmt.Sprintf("start %v@%v", cb["type"], p["index"]))
		case "content_block_stop":
			out = append(out, fmt.Sprintf("stop @%v", p["index"]))
		}
	}
	return out
}

// chatFixtureSSE returns a testdata/<name>.sse body for the proxy tests.
func chatFixtureSSE(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".sse"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Records what the converter does today when reasoning arrives after a tool call
// has started: the thinking block opens while the tool block is still open. The
// tool input is unaffected, because its deltas carry the tool block's own index.
func TestChatFixture_ReasoningAfterToolStarted(t *testing.T) {
	sink := replayChat(t, "chat_reasoning_after_tool")
	assertGrammar(t, sink)
	want := []string{"start tool_use@0", "start thinking@1", "stop @1", "stop @0"}
	got := blockEvents(sink)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("block events = %v, want %v", got, want)
	}
	if n := maxOpenBlocks(sink); n != 2 {
		t.Fatalf("%d blocks open at once, want 2 (tool and thinking overlap)", n)
	}
	for i, ev := range sink.events {
		p, _ := sink.payloads[i].(map[string]any)
		d, _ := p["delta"].(map[string]any)
		if ev == "content_block_delta" && d["type"] == "input_json_delta" && p["index"] != 0 {
			t.Fatalf("tool arguments attached to block %v, want the tool block 0", p["index"])
		}
	}
	if args := collectDeltas(sink, "input_json_delta", "partial_json"); args != `{"key":"alpha"}` {
		t.Fatalf("tool args = %q", args)
	}
	if got := collectDeltas(sink, "thinking_delta", "thinking"); got != "checking the key" {
		t.Fatalf("thinking = %q", got)
	}
	if sr := stopReason(t, sink); sr != "tool_use" {
		t.Fatalf("stop_reason = %q", sr)
	}

	// The stream:false assembler orders blocks by index and keeps the full input.
	r := cpPost(t, cpProxy(t, config.ProtocolOpenAIChat, chatFixtureSSE(t, "chat_reasoning_after_tool")), false)
	if r.status != http.StatusOK || r.body["stop_reason"] != "tool_use" {
		t.Fatalf("status %d: %s", r.status, r.raw)
	}
	content, _ := r.body["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %s", r.raw)
	}
	tu, _ := content[0].(map[string]any)
	in, _ := tu["input"].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "lookup" || len(in) != 1 || in["key"] != "alpha" {
		t.Fatalf("tool_use block = %v", tu)
	}
	th, _ := content[1].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "checking the key" || th["signature"] != "" {
		t.Fatalf("thinking block = %v", th)
	}
}
