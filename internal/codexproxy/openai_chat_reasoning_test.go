package codexproxy

import "testing"

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
