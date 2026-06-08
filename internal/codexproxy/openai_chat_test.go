package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parseReq(t *testing.T, raw string) *anthropicRequest {
	t.Helper()
	var a anthropicRequest
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return &a
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not a map: %#v", v)
	}
	return m
}

// ---- request translation ---------------------------------------------------

func TestChatTranslate_SystemAndText(t *testing.T) {
	a := parseReq(t, `{"model":"m","max_tokens":10,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	r, err := translateChatRequest(a)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Stream || r.StreamOptions == nil || !r.StreamOptions.IncludeUsage {
		t.Fatal("stream + include_usage must be set")
	}
	if r.MaxTokens != 10 {
		t.Fatalf("max_tokens = %d", r.MaxTokens)
	}
	if len(r.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(r.Messages))
	}
	sys := asMap(t, r.Messages[0])
	if sys["role"] != "system" || sys["content"] != "be brief" {
		t.Fatalf("system msg = %v", sys)
	}
	usr := asMap(t, r.Messages[1])
	if usr["role"] != "user" || usr["content"] != "hi" {
		t.Fatalf("user msg = %v", usr)
	}
}

func TestChatTranslate_AssistantTextAndToolUse(t *testing.T) {
	content := `[{"type":"text","text":"let me check"},{"type":"tool_use","id":"t1","name":"lookup","input":{"k":"v"}}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":`+content+`}]}`)
	r, _ := translateChatRequest(a)
	if len(r.Messages) != 1 {
		t.Fatalf("a text+tool_use assistant turn must be ONE message, got %d", len(r.Messages))
	}
	m := asMap(t, r.Messages[0])
	if m["role"] != "assistant" || m["content"] != "let me check" {
		t.Fatalf("assistant msg = %v", m)
	}
	calls, ok := m["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", m["tool_calls"])
	}
	fn := asMap(t, calls[0]["function"])
	if calls[0]["id"] != "t1" || fn["name"] != "lookup" || fn["arguments"] != `{"k":"v"}` {
		t.Fatalf("tool_call = %v", calls[0])
	}
}

func TestChatTranslate_ToolResultOrderedFirst(t *testing.T) {
	content := `[{"type":"tool_result","tool_use_id":"t1","content":"the result"},{"type":"text","text":"thanks"}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":`+content+`}]}`)
	r, _ := translateChatRequest(a)
	if len(r.Messages) != 2 {
		t.Fatalf("messages = %d, want tool then user", len(r.Messages))
	}
	tool := asMap(t, r.Messages[0])
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" || tool["content"] != "the result" {
		t.Fatalf("tool msg must come first: %v", tool)
	}
	usr := asMap(t, r.Messages[1])
	if usr["role"] != "user" || usr["content"] != "thanks" {
		t.Fatalf("trailing user text = %v", usr)
	}
}

func TestChatTranslate_ToolChoiceAndTools(t *testing.T) {
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`)
	r, _ := translateChatRequest(a)
	if len(r.Tools) != 1 || r.Tools[0].Function.Name != "lookup" {
		t.Fatalf("tools = %#v", r.Tools)
	}
	if r.ToolChoice != "required" {
		t.Fatalf("tool_choice = %v, want required", r.ToolChoice)
	}
	if r.ParallelToolCalls == nil || *r.ParallelToolCalls != false {
		t.Fatalf("disable_parallel_tool_use must set parallel_tool_calls=false")
	}
}

func TestChatTranslate_Image(t *testing.T) {
	content := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"see"}]`
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":`+content+`}]}`)
	r, _ := translateChatRequest(a)
	m := asMap(t, r.Messages[0])
	parts, ok := m["content"].([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("image message content must be a parts array: %#v", m["content"])
	}
	img := asMap(t, parts[0])
	if img["type"] != "image_url" || asMap(t, img["image_url"])["url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image part = %v", img)
	}
}

// ---- streaming conversion --------------------------------------------------

func replayChat(t *testing.T, name string) *recSink {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".sse"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &recSink{}
	if err := newChatStreamConverter(sink, "gpt-x").Convert(bytes.NewReader(b)); err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return sink
}

func TestChatFixture_Text(t *testing.T) {
	sink := replayChat(t, "chat_text")
	assertGrammar(t, sink)
	if got := collectDeltas(sink, "text_delta", "text"); got != "pong" {
		t.Fatalf("text = %q", got)
	}
	if sr := stopReason(t, sink); sr != "end_turn" {
		t.Fatalf("stop_reason = %q", sr)
	}
	usage, _ := sink.payload("message_delta", 0)["usage"].(map[string]any)
	if usage["input_tokens"] != 12 || usage["output_tokens"] != 3 {
		t.Fatalf("usage = %v", usage)
	}
}

func TestChatFixture_StreamedToolCall(t *testing.T) {
	sink := replayChat(t, "chat_tool")
	assertGrammar(t, sink)
	args := collectDeltas(sink, "input_json_delta", "partial_json")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil || parsed["key"] != "alpha" {
		t.Fatalf("streamed tool args %q: %v", args, err)
	}
	if sr := stopReason(t, sink); sr != "tool_use" {
		t.Fatalf("stop_reason = %q", sr)
	}
}

func TestChatFixture_ParallelTools(t *testing.T) {
	sink := replayChat(t, "chat_parallel")
	assertGrammar(t, sink)
	names := map[string]bool{}
	for i, ev := range sink.events {
		if ev != "content_block_start" {
			continue
		}
		cb, _ := sink.payloads[i].(map[string]any)
		block, _ := cb["content_block"].(map[string]any)
		if block["type"] == "tool_use" {
			names[block["name"].(string)] = true
		}
	}
	if !names["f0"] || !names["f1"] {
		t.Fatalf("both parallel tool blocks must open: %v", names)
	}
}

func TestChatFixture_LengthStop(t *testing.T) {
	sink := replayChat(t, "chat_length")
	assertGrammar(t, sink)
	if sr := stopReason(t, sink); sr != "max_tokens" {
		t.Fatalf("finish_reason length must map to max_tokens, got %q", sr)
	}
}

// call hits <upstream>/chat/completions with a Bearer key and streams a real
// chat backend's response; a non-2xx body that echoes the key is redacted (here
// with a key that does NOT match the generic pattern, exercising the exact-key
// path) before it becomes an upstreamError.
func TestOpenAIChatUpstream_CallConvertAndRedact(t *testing.T) {
	const key = "raw-vendor-key-9999" // not an sk-/Bearer shape: only exact-match redaction catches it
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer ok.Close()
	u := newOpenAIChatUpstream(ok.URL + "/v1")
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	body, err := u.call(context.Background(), a, key)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	sink := &recSink{}
	_ = u.convert(body, sink, "m", key)
	body.Close()
	if got := collectDeltas(sink, "text_delta", "text"); got != "hi" {
		t.Fatalf("converted text = %q", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key " + key + " rejected"))
	}))
	defer bad.Close()
	_, err = newOpenAIChatUpstream(bad.URL+"/v1").call(context.Background(), a, key)
	ue, ok2 := err.(*upstreamError)
	if !ok2 || ue.kind != upAuth {
		t.Fatalf("want upAuth upstreamError, got %v", err)
	}
	if strings.Contains(ue.message, key) {
		t.Fatalf("upstreamError leaked the key: %q", ue.message)
	}
}

// A mid-stream error chunk ends on a redacted Anthropic error event (open blocks
// closed), never a clean message_stop, and never leaks the echoed key.
func TestChatFixture_ErrorRedacted(t *testing.T) {
	sink := replayChat(t, "chat_error")
	last := sink.events[len(sink.events)-1]
	if last != "error" {
		t.Fatalf("stream must end on an error event, got %q (%s)", last, sink.seq())
	}
	if strings.Contains(sink.seq(), "message_stop") {
		t.Fatal("an errored stream must not emit a clean message_stop")
	}
	errPayload := sink.payload("error", 0)
	em, _ := errPayload["error"].(map[string]any)
	msg, _ := em["message"].(string)
	if strings.Contains(msg, "SECRET12345678") {
		t.Fatalf("error leaked the key: %q", msg)
	}
	if !strings.Contains(msg, "REDACTED") {
		t.Fatalf("error must be redacted: %q", msg)
	}
	// the open text block must have been closed before the error.
	if sink.payload("content_block_stop", 0) == nil {
		t.Fatal("open text block must be closed before the error event")
	}
}
