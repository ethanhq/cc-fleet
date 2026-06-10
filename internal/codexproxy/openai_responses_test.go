package codexproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The openai-responses upstream sends to <upstream>/responses with a Bearer key,
// maps Anthropic max_tokens -> max_output_tokens, drops the codex OpenAI-Beta
// header, and converts the Responses stream with the shared converter.
func TestOpenAIResponsesUpstream_CallMapsMaxOutput(t *testing.T) {
	var gotBody map[string]any
	var gotBeta, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBeta = r.Header.Get("OpenAI-Beta")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","item":{"id":"m1","type":"message"}}

data: {"type":"response.output_text.delta","item_id":"m1","delta":"yo"}

data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":2}}}

`))
	}))
	defer srv.Close()

	u := newOpenAIResponsesUpstream(srv.URL + "/v1")
	a := parseReq(t, `{"model":"gpt-x","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	body, err := u.call(context.Background(), a, newConvCtx(a, "sk-billed-123"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	sink := &recSink{}
	_ = u.convert(body, sink, ccKey("gpt-x", "sk-billed-123"))
	body.Close()

	if gotPath != "/v1/responses" {
		t.Fatalf("path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-billed-123" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBeta != "" {
		t.Fatalf("openai-responses must not send the codex OpenAI-Beta header, got %q", gotBeta)
	}
	if gotBody["max_output_tokens"] != float64(256) {
		t.Fatalf("max_output_tokens = %v, want 256", gotBody["max_output_tokens"])
	}
	if got := collectDeltas(sink, "text_delta", "text"); got != "yo" {
		t.Fatalf("converted text = %q", got)
	}
}

// translateRequest (the codex path) never sets max_output_tokens — the ChatGPT
// backend 400s on it, so omitempty must drop it.
func TestResponsesTranslate_CodexOmitsMaxOutput(t *testing.T) {
	a := parseReq(t, `{"model":"m","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	r, _ := translateRequest(a, newConvCtx(a, ""))
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "max_output_tokens") {
		t.Fatalf("codex request must omit max_output_tokens: %s", b)
	}
}

// A streaming error chunk that echoes a non-pattern key is redacted via the
// upstream's per-request convert redactor (the exact-key path), for both the
// Chat and Responses converters.
func TestOpenAIConvert_RedactsStreamingErrorKey(t *testing.T) {
	const key = "raw-provider-key-9999" // not an sk-/Bearer shape

	chatBody := `data: {"error":{"message":"upstream rejected ` + key + ` here"}}` + "\n\n"
	csink := &recSink{}
	_ = newOpenAIChatUpstream("https://x/v1").convert(strings.NewReader(chatBody), csink, ccKey("m", key))
	if msg := errMessage(csink); strings.Contains(msg, key) {
		t.Fatalf("chat streaming error leaked the key: %q", msg)
	}

	respBody := `data: {"type":"response.failed","response":{"error":{"message":"bad ` + key + ` token"}}}` + "\n\n"
	rsink := &recSink{}
	_ = newOpenAIResponsesUpstream("https://x/v1").convert(strings.NewReader(respBody), rsink, ccKey("m", key))
	if msg := errMessage(rsink); strings.Contains(msg, key) {
		t.Fatalf("responses streaming error leaked the key: %q", msg)
	}
}

func errMessage(sink *recSink) string {
	p := sink.payload("error", 0)
	em, _ := p["error"].(map[string]any)
	s, _ := em["message"].(string)
	return s
}

// reasoning_text.* maps to the thinking block and refusal.* is surfaced as text —
// neither is silently dropped (the two MUST-VERIFY stream shapes).
func TestResponsesConverter_ReasoningTextAndRefusal(t *testing.T) {
	sse := `data: {"type":"response.output_item.added","item":{"id":"r1","type":"reasoning"}}

data: {"type":"response.reasoning_text.delta","item_id":"r1","delta":"thinking"}

data: {"type":"response.output_item.done","item":{"id":"r1","type":"reasoning"}}

data: {"type":"response.output_item.added","item":{"id":"m1","type":"message"}}

data: {"type":"response.refusal.delta","item_id":"m1","delta":"cannot help"}

data: {"type":"response.output_item.done","item":{"id":"m1","type":"message"}}

data: {"type":"response.completed","response":{"status":"completed"}}

`
	sink := runConvert(t, sse)
	assertGrammar(t, sink)
	if got := collectDeltas(sink, "thinking_delta", "thinking"); got != "thinking" {
		t.Fatalf("reasoning_text must map to thinking: %q", got)
	}
	if got := collectDeltas(sink, "text_delta", "text"); got != "cannot help" {
		t.Fatalf("refusal must surface as text: %q", got)
	}
}
