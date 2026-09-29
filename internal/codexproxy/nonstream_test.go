package codexproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// cpProxy serves sse from a loopback fake upstream and returns the URL of a proxy
// handler running the real upstream + converter for protocol in front of it.
func cpProxy(t *testing.T, protocol, sse string) string {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	t.Cleanup(fake.Close)
	var up upstream = newOpenAIChatUpstream(fake.URL)
	if protocol == config.ProtocolOpenAIResponses {
		up = newOpenAIResponsesUpstream(fake.URL)
	}
	return cpServe(t, up, protocol)
}

func cpServe(t *testing.T, up upstream, protocol string) string {
	t.Helper()
	s := httptest.NewServer(newServer(up, protocol, "").handler())
	t.Cleanup(s.Close)
	return s.URL
}

type cpResp struct {
	status int
	ctype  string
	raw    string
	body   map[string]any
}

// cpPost sends a /v1/messages request with the given stream flag and a fake key.
func cpPost(t *testing.T, base string, stream bool) cpResp {
	t.Helper()
	body := fmt.Sprintf(`{"model":"m","max_tokens":10,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-test-cp-fake")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	r := cpResp{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"), raw: string(b)}
	if !stream {
		if err := json.Unmarshal(b, &r.body); err != nil {
			t.Fatalf("non-stream body is not JSON: %v: %s", err, b)
		}
	}
	return r
}

func cpErrType(t *testing.T, r cpResp) string {
	t.Helper()
	e, _ := r.body["error"].(map[string]any)
	if r.body["type"] != "error" || e == nil {
		t.Fatalf("not an Anthropic error: %s", r.raw)
	}
	s, _ := e["type"].(string)
	return s
}

var cpMsgID = regexp.MustCompile(`^msg_[0-9a-f]{24}$`)

func TestNonStreamAssemblesMessage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"thinking it over"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"sig-cp"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"m_1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"m_1","delta":"hel"}`,
		`data: {"type":"response.output_text.delta","item_id":"m_1","delta":"lo"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"m_1","type":"message"}}`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_cp","name":"lookup"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"key\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"alpha\"}"}`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":30,"input_tokens_details":{"cached_tokens":10},"output_tokens":7}}}`,
		"", "",
	}, "\n\n")
	r := cpPost(t, cpProxy(t, config.ProtocolOpenAIResponses, sse), false)
	if r.status != http.StatusOK || !strings.HasPrefix(r.ctype, "application/json") {
		t.Fatalf("status %d, content-type %q: %s", r.status, r.ctype, r.raw)
	}
	m := r.body
	if id, _ := m["id"].(string); !cpMsgID.MatchString(id) {
		t.Fatalf("id = %v", m["id"])
	}
	if m["type"] != "message" || m["role"] != "assistant" || m["model"] != "m" {
		t.Fatalf("envelope = %s", r.raw)
	}
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", m["stop_reason"])
	}
	if _, ok := m["stop_sequence"]; !ok {
		t.Fatalf("stop_sequence missing: %s", r.raw)
	}
	usage, _ := m["usage"].(map[string]any)
	if usage["input_tokens"] != 20.0 || usage["output_tokens"] != 7.0 || usage["cache_read_input_tokens"] != 10.0 {
		t.Fatalf("usage = %v", usage)
	}
	content, _ := m["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %s", r.raw)
	}
	th, _ := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "thinking it over" || th["signature"] != "sig-cp" {
		t.Fatalf("thinking block = %v", th)
	}
	tx, _ := content[1].(map[string]any)
	if tx["type"] != "text" || tx["text"] != "hello" {
		t.Fatalf("text block = %v", tx)
	}
	tu, _ := content[2].(map[string]any)
	in, _ := tu["input"].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_cp" || tu["name"] != "lookup" || in["key"] != "alpha" {
		t.Fatalf("tool_use block = %v", tu)
	}

	// The chat converter assembles too: text, end_turn and usage.
	chat := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"pong"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
		`data: [DONE]`, "",
	}, "\n\n")
	r = cpPost(t, cpProxy(t, config.ProtocolOpenAIChat, chat), false)
	if r.status != http.StatusOK || r.body["stop_reason"] != "end_turn" {
		t.Fatalf("chat: status %d: %s", r.status, r.raw)
	}
	content, _ = r.body["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "pong" {
		t.Fatalf("chat content = %s", r.raw)
	}
	if u, _ := r.body["usage"].(map[string]any); u["input_tokens"] != 12.0 || u["output_tokens"] != 3.0 {
		t.Fatalf("chat usage = %s", r.raw)
	}
}

func TestNonStreamErrorMapsStatus(t *testing.T) {
	chatErr := func(code string) string {
		return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"part\"},\"finish_reason\":null}]}\n\n" +
			`data: {"error":{"message":"Provider returned error","code":` + code + "}}\n\ndata: [DONE]\n\n"
	}
	cases := []struct {
		name, protocol, sse string
		status              int
		etype               string
	}{
		{"chat 429", config.ProtocolOpenAIChat, chatErr("429"), 429, "rate_limit_error"},
		{"chat string 429", config.ProtocolOpenAIChat, chatErr(`"429"`), 429, "rate_limit_error"},
		{"chat 401", config.ProtocolOpenAIChat, chatErr("401"), 401, "authentication_error"},
		{"chat 403", config.ProtocolOpenAIChat, chatErr("403"), 401, "authentication_error"},
		{"chat 500", config.ProtocolOpenAIChat, chatErr("500"), 502, "api_error"},
		{"chat no code", config.ProtocolOpenAIChat, chatErr("null"), 502, "api_error"},
		{"responses quota", config.ProtocolOpenAIResponses,
			`data: {"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"slow down"}}}` + "\n\n", 429, "rate_limit_error"},
		{"responses other", config.ProtocolOpenAIResponses,
			`data: {"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}` + "\n\n", 502, "api_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := cpProxy(t, tc.protocol, tc.sse)
			r := cpPost(t, base, false)
			if r.status != tc.status || cpErrType(t, r) != tc.etype {
				t.Fatalf("got %d %s, want %d %s", r.status, r.raw, tc.status, tc.etype)
			}
			if msg, _ := r.body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "upstream") {
				t.Fatalf("message should carry the upstream error: %q", msg)
			}
			// The same upstream error on the streaming path is unchanged: an SSE
			// error event of type api_error on a 200 stream.
			s := cpPost(t, base, true)
			if s.status != http.StatusOK || !strings.HasPrefix(s.ctype, "text/event-stream") ||
				!strings.Contains(s.raw, "event: error") || !strings.Contains(s.raw, `"type":"api_error"`) ||
				strings.Contains(s.raw, "rate_limit_error") || strings.Contains(s.raw, "authentication_error") {
				t.Fatalf("SSE path changed: %d %q %s", s.status, s.ctype, s.raw)
			}
		})
	}
}

func TestNonStreamToolJSONCutBy429(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"key\":\"al"}}]},"finish_reason":null}]}`,
		`data: {"error":{"message":"Provider returned error","code":429}}`,
		`data: [DONE]`, "",
	}, "\n\n")
	r := cpPost(t, cpProxy(t, config.ProtocolOpenAIChat, sse), false)
	if r.status != http.StatusTooManyRequests || cpErrType(t, r) != "rate_limit_error" {
		t.Fatalf("a 429 cutting tool JSON must win over the assembly error: %d %s", r.status, r.raw)
	}
}

func TestNonStreamBadToolJSON502(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"key\":oops"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, "",
	}, "\n\n")
	r := cpPost(t, cpProxy(t, config.ProtocolOpenAIChat, sse), false)
	if r.status != http.StatusBadGateway || cpErrType(t, r) != "api_error" {
		t.Fatalf("invalid tool JSON must be a 502 api_error: %d %s", r.status, r.raw)
	}
}

func TestNonStreamNullToolArgs502(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"null"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, "",
	}, "\n\n")
	r := cpPost(t, cpProxy(t, config.ProtocolOpenAIChat, sse), false)
	if r.status != http.StatusBadGateway || cpErrType(t, r) != "api_error" {
		t.Fatalf("null tool arguments must be a 502 api_error: %d %s", r.status, r.raw)
	}
}

// cpIncompleteUpstream starts a message but never finishes it and reports no error.
type cpIncompleteUpstream struct{}

func (cpIncompleteUpstream) call(context.Context, *anthropicRequest, *convCtx) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (cpIncompleteUpstream) convert(_ io.Reader, sink sseSink, _ *convCtx) error {
	_ = sink.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_x", "model": "m"}})
	_ = sink.event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	return sink.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "half"}})
}

func (cpIncompleteUpstream) models() []string { return nil }

func TestNonStreamIncomplete502(t *testing.T) {
	r := cpPost(t, cpServe(t, cpIncompleteUpstream{}, config.ProtocolOpenAIChat), false)
	if r.status != http.StatusBadGateway || cpErrType(t, r) != "api_error" {
		t.Fatalf("an unfinished message must be a 502 api_error: %d %s", r.status, r.raw)
	}
	if msg, _ := r.body["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "ended before the message completed") {
		t.Fatalf("message = %q", msg)
	}
}
