package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	r, err := translateChatRequest(a, newConvCtx(a, ""))
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
	r, _ := translateChatRequest(a, newConvCtx(a, ""))
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
	r, _ := translateChatRequest(a, newConvCtx(a, ""))
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
	r, _ := translateChatRequest(a, newConvCtx(a, ""))
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
	r, _ := translateChatRequest(a, newConvCtx(a, ""))
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
	if err := newChatStreamConverter(sink, ccTest("gpt-x")).Convert(bytes.NewReader(b)); err != nil {
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
	const key = "raw-provider-key-9999" // not an sk-/Bearer shape: only exact-match redaction catches it
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
	body, err := u.call(context.Background(), a, newConvCtx(a, key))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	sink := &recSink{}
	_ = u.convert(body, sink, ccKey("m", key))
	body.Close()
	if got := collectDeltas(sink, "text_delta", "text"); got != "hi" {
		t.Fatalf("converted text = %q", got)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid key " + key + " rejected"))
	}))
	defer bad.Close()
	_, err = newOpenAIChatUpstream(bad.URL+"/v1").call(context.Background(), a, newConvCtx(a, key))
	ue, ok2 := err.(*upstreamError)
	if !ok2 || ue.kind != upAuth {
		t.Fatalf("want upAuth upstreamError, got %v", err)
	}
	if strings.Contains(ue.message, key) {
		t.Fatalf("upstreamError leaked the key: %q", ue.message)
	}
}

// Fixed pieces of a realistic Claude Code metadata.user_id, which is a JSON
// string carrying the install's device id and the Claude account uuid next to
// the session id.
const (
	testDeviceID    = "d3v1ce-9f2a41c7e05b"
	testAccountUUID = "acc0unt-5b1e0c3d-77aa-4c0e"
)

func claudeUserID(sessionID string) string {
	return `{"device_id":"` + testDeviceID + `","account_uuid":"` + testAccountUUID + `","session_id":"` + sessionID + `"}`
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// reqWithUserID builds a request whose metadata.user_id is userID verbatim;
// marshalling escapes it, so a JSON-object user_id reaches the request as a string.
func reqWithUserID(t *testing.T, userID string) *anthropicRequest {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":      "m",
		"max_tokens": 10,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"metadata":   map[string]string{"user_id": userID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return parseReq(t, string(raw))
}

// headerServer answers every request with an empty SSE stream; seen returns the
// headers of the latest request it received.
func headerServer(t *testing.T) (srv *httptest.Server, seen func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var last http.Header
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// sendReq sends a through up and returns the headers headerServer saw.
func sendReq(t *testing.T, up upstream, seen func() http.Header, a *anthropicRequest) http.Header {
	t.Helper()
	body, err := up.call(context.Background(), a, newConvCtx(a, "key"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	body.Close()
	return seen()
}

// call must attach x-opencode-session on an opencode.ai upstream (OpenCode Go's
// gateway has rejected headerless requests with 400 MissingSessionID since
// 2026-09-06). The value is an opaque digest per conversation: metadata.user_id
// also carries the device id and account uuid, which must not reach the gateway.
func TestOpenAIChatUpstream_OpenCodeGoSessionHeader(t *testing.T) {
	srv, seen := headerServer(t)

	// httptest's server is 127.0.0.1, not opencode.ai, so point baseURL's Host at
	// opencode.ai while dialing the real test listener via a custom transport —
	// simplest is to swap in a request-URL rewrite via http.Client.Transport.
	u := newOpenAIChatUpstream("https://opencode.ai/zen/go/v1")
	u.http.Transport = rewriteHostTransport{targetURL: srv.URL}

	const sessionID = "11111111-2222-4333-8444-555555555555"
	got := sendReq(t, u, seen, reqWithUserID(t, claudeUserID(sessionID))).Get("x-opencode-session")
	if !hex32.MatchString(got) {
		t.Fatalf("x-opencode-session = %q, want 32 lowercase hex chars", got)
	}
	for _, leaked := range []string{testDeviceID, testAccountUUID, sessionID} {
		if strings.Contains(got, leaked) {
			t.Fatalf("x-opencode-session = %q leaks %q", got, leaked)
		}
	}
	if again := sendReq(t, u, seen, reqWithUserID(t, claudeUserID(sessionID))).Get("x-opencode-session"); again != got {
		t.Fatalf("same user_id sent %q then %q, want a stable value", got, again)
	}
	other := sendReq(t, u, seen, reqWithUserID(t, claudeUserID("66666666-7777-4888-8999-000000000000"))).Get("x-opencode-session")
	if other == got {
		t.Fatalf("a different session_id sent the same value %q", got)
	}
}

// The header value depends on the session id alone: the other fields Claude Code
// puts in metadata.user_id do not change it. An older client's plain string keeps
// one stable value too.
func TestOpenCodeSessionID_SessionIDOnly(t *testing.T) {
	const sessionID = "11111111-2222-4333-8444-555555555555"
	const want = "cf4c4732fd3b8f8a55b60871950a2f22" // hex(sha256(sessionID)[:16])
	for _, userID := range []string{
		claudeUserID(sessionID),
		`{"device_id":"another-device","account_uuid":"another-account","session_id":"` + sessionID + `"}`,
		`{"session_id":"` + sessionID + `","parent_session_id":"99999999-aaaa-4bbb-8ccc-dddddddddddd","device_id":"` + testDeviceID + `"}`,
	} {
		if got := opencodeSessionID(reqWithUserID(t, userID)); got != want {
			t.Errorf("user_id %s: x-opencode-session = %q, want %q", userID, got, want)
		}
	}
	if got := opencodeSessionID(reqWithUserID(t, "user_abc")); got != "6bb79e55dbc86636062d3c7466f4c463" {
		t.Errorf("plain-string user_id: x-opencode-session = %q", got)
	}
}

// Without metadata.user_id, call must still send a non-empty x-opencode-session
// (a header present but empty is exactly the condition OpenCode Go rejects) —
// covering claude requests that, however unobserved in practice, omit it.
func TestOpenAIChatUpstream_OpenCodeGoSessionHeaderFallback(t *testing.T) {
	srv, seen := headerServer(t)

	u := newOpenAIChatUpstream("https://opencode.ai/zen/go/v1")
	u.http.Transport = rewriteHostTransport{targetURL: srv.URL}

	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	first := sendReq(t, u, seen, a).Get("x-opencode-session")
	if first == "" {
		t.Fatal("x-opencode-session must never be empty on an opencode.ai call")
	}
	if second := sendReq(t, u, seen, a).Get("x-opencode-session"); second == first {
		t.Fatalf("two calls without metadata sent the same value %q", first)
	}
}

// A non-OpenCode-Go openai-chat upstream (a direct provider API, some other
// aggregator) must NOT get x-opencode-session — it's an OpenCode Go-specific
// requirement, not a general Chat Completions convention.
func TestOpenAIChatUpstream_NoSessionHeaderForOtherHosts(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get("x-opencode-session") != ""
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	u := newOpenAIChatUpstream(srv.URL + "/v1")
	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"user_abc"}}`)
	body, err := u.call(context.Background(), a, newConvCtx(a, "key"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	body.Close()
	if sawHeader {
		t.Fatal("x-opencode-session must not be sent to a non-opencode.ai upstream")
	}
}

func TestIsOpenCodeGoHost(t *testing.T) {
	cases := []struct {
		baseURL string
		want    bool
	}{
		{"https://opencode.ai/zen/go/v1", true},
		{"https://foo.opencode.ai/v1", true},
		{"https://OpenCode.AI/zen/go/v1", true},      // hostnames are case-insensitive
		{"https://opencode.ai./zen/go/v1", true},     // trailing FQDN dot
		{"https://opencode.ai:8443/zen/go/v1", true}, // non-standard port
		{"opencode.ai/zen/go/v1", false},             // no scheme: url.Parse treats this as a path, no Host
		{"https://api.openai.com/v1", false},
		{"https://notopencode.ai.evil.example.com/v1", false},
		{"https://evil.com@opencode.ai/v1", true},  // userinfo before @ is not the host
		{"https://opencode.ai@evil.com/v1", false}, // userinfo trick the other way must NOT match
		{"https://127.0.0.1:17223/", false},
		{"https://[::1%25a.opencode.ai]/v1", false}, // an IPv6 zone, not a subdomain
		{":::not a url", false},
	}
	for _, c := range cases {
		if got := isOpenCodeGoHost(c.baseURL); got != c.want {
			t.Errorf("isOpenCodeGoHost(%q) = %v, want %v", c.baseURL, got, c.want)
		}
	}
}

// A metadata.user_id containing CR/LF must never reach the wire as (or corrupt)
// the x-opencode-session header — only a digest of it is sent, so a malformed
// client-supplied value can neither inject a header nor fail the transport.
func TestOpenAIChatUpstream_HostileUserIDIsDigestedNotInjected(t *testing.T) {
	srv, seen := headerServer(t)

	u := newOpenAIChatUpstream("https://opencode.ai/zen/go/v1")
	u.http.Transport = rewriteHostTransport{targetURL: srv.URL}

	h := sendReq(t, u, seen, reqWithUserID(t, "line1\r\nX-Injected: evil"))
	if got := h.Get("x-opencode-session"); !hex32.MatchString(got) {
		t.Fatalf("x-opencode-session = %q, want 32 lowercase hex chars", got)
	}
	if v := h.Values("X-Injected"); len(v) != 0 {
		t.Fatalf("injected header reached the upstream: %q", v)
	}
	if h.Get("Authorization") == "" {
		t.Fatal("request must still reach the upstream with its normal headers")
	}
}

// rewriteHostTransport redials every request at targetURL — the *struct's*
// baseURL field (what isOpenCodeGoHost/isOpenCodeGo actually key off) stays
// "https://opencode.ai/...", only the outgoing TCP destination and Host header
// change — so a test can assert host-based behavior against a real
// opencode.ai-shaped baseURL while actually talking to a local httptest server.
type rewriteHostTransport struct{ targetURL string }

func (t rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, err := url.Parse(t.targetURL)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	return http.DefaultTransport.RoundTrip(req)
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

// A finish_reason of tool_calls with NO streamed tool_call deltas (a malformed /
// empty upstream turn) must not stop tool_use — a tool_use stop with zero
// tool_use blocks would hand the client an unkeepable promise.
func TestChatFixture_ToolCallsFinishWithoutToolBlocks(t *testing.T) {
	sink := replayChat(t, "chat_tool_calls_empty")
	assertGrammar(t, sink)
	if sr := stopReason(t, sink); sr != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn (no tool block ever opened)", sr)
	}
}
