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

// ---- item 1: thinking-effort mapping ---------------------------------------

func reasoningOf(t *testing.T, raw string) *reasoningOpt {
	t.Helper()
	a := parseReq(t, raw)
	r, err := translateRequest(a, newConvCtx(a, ""))
	if err != nil {
		t.Fatal(err)
	}
	return r.Reasoning
}

func TestEffort_OutputConfigMapping(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"low", "low"}, {"medium", "medium"}, {"high", "high"}, {"xhigh", "xhigh"}, {"max", "xhigh"},
	} {
		r := reasoningOf(t, `{"model":"gpt-5.5","thinking":{"type":"adaptive"},"output_config":{"effort":"`+c.in+`"},"messages":[{"role":"user","content":"hi"}]}`)
		if r == nil || r.Effort != c.want {
			t.Fatalf("output_config.effort %q -> %v, want effort %q", c.in, r, c.want)
		}
	}
}

func TestEffort_AbsentOmitsEffortButKeepsReasoning(t *testing.T) {
	a := parseReq(t, `{"model":"gpt-5.5","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}]}`)
	r, _ := translateRequest(a, newConvCtx(a, ""))
	if r.Reasoning == nil {
		t.Fatal("adaptive thinking must still request reasoning (summary + encrypted_content)")
	}
	if r.Reasoning.Effort != "" {
		t.Fatalf("absent effort must be empty (backend default), got %q", r.Reasoning.Effort)
	}
	if b, _ := json.Marshal(r); strings.Contains(string(b), `"effort"`) {
		t.Fatalf("absent effort must be OMITTED on the wire: %s", b)
	}
}

func TestEffort_LegacyBudgetFallback(t *testing.T) {
	r := reasoningOf(t, `{"model":"m","thinking":{"type":"enabled","budget_tokens":20000},"messages":[{"role":"user","content":"hi"}]}`)
	if r == nil || r.Effort != "high" {
		t.Fatalf("legacy budget 20000 must bucket to high, got %v", r)
	}
}

// The openai-responses lane steps xhigh down to high (not every api.openai.com model
// accepts xhigh); the shared translator emits the canonical xhigh, the upstream clamps.
func TestEffort_OpenAIResponsesClampsXhigh(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"))
	}))
	defer srv.Close()
	u := newOpenAIResponsesUpstream(srv.URL + "/v1")
	a := parseReq(t, `{"model":"gpt-x","thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`)
	body, err := u.call(context.Background(), a, newConvCtx(a, "sk-1"))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	body.Close()
	reasoning, _ := gotBody["reasoning"].(map[string]any)
	if reasoning == nil || reasoning["effort"] != "high" {
		t.Fatalf("openai-responses must clamp xhigh -> high, got reasoning=%v", reasoning)
	}
}

// ---- item 2: prompt-cache stability (billing header strip) -----------------

func TestSystemText_StripsBillingHeader(t *testing.T) {
	sys := `[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.37; cch=ABC123;"},{"type":"text","text":"real system prompt"}]`
	a := parseReq(t, `{"model":"m","system":`+sys+`,"messages":[{"role":"user","content":"hi"}]}`)
	r, _ := translateRequest(a, newConvCtx(a, ""))
	if strings.Contains(r.Instructions, "billing-header") || strings.Contains(r.Instructions, "cch=") {
		t.Fatalf("the volatile billing header must be stripped: %q", r.Instructions)
	}
	if r.Instructions != "real system prompt" {
		t.Fatalf("the real system prompt must survive intact: %q", r.Instructions)
	}
}

// ---- item 4: tool_result.is_error ------------------------------------------

func TestToolResult_IsErrorPrefixed(t *testing.T) {
	const content = `[{"type":"tool_result","tool_use_id":"t1","content":"boom","is_error":true}]`
	items, _ := translateMessage(anthropicMessage{Role: "user", Content: json.RawMessage(content)}, &convCtx{})
	if out := items[0].(map[string]any)["output"].(string); !strings.HasPrefix(out, "[tool error]") {
		t.Fatalf("Responses is_error result must be prefixed, got %q", out)
	}
	citems, _ := translateChatMessage(anthropicMessage{Role: "user", Content: json.RawMessage(content)}, &convCtx{})
	if out := citems[0].(map[string]any)["content"].(string); !strings.HasPrefix(out, "[tool error]") {
		t.Fatalf("Chat is_error result must be prefixed, got %q", out)
	}
	// a non-error result is unprefixed
	ok, _ := translateMessage(anthropicMessage{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"fine"}]`)}, &convCtx{})
	if out := ok[0].(map[string]any)["output"].(string); out != "fine" {
		t.Fatalf("a successful tool_result must be unprefixed, got %q", out)
	}
}

// ---- item 5: tool_choice none on the Responses lane ------------------------

func TestToolChoice_NoneResponses(t *testing.T) {
	a := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"none"}}`)
	r, _ := translateRequest(a, newConvCtx(a, ""))
	if r.ToolChoice != "none" {
		t.Fatalf("tool_choice none must map to none (not auto), got %v", r.ToolChoice)
	}
}

// ---- item 3: tool-name sanitize + restore ----------------------------------

func TestToolNames_SanitizeRequestRestoreResponse(t *testing.T) {
	const orig = "mcp.server.do-it" // dotted -> non-conforming
	a := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"`+orig+`","input_schema":{"type":"object"}}]}`)
	cc := newConvCtx(a, "")
	r, _ := translateRequest(a, cc)
	sent := r.Tools[0].Name
	if sent == orig || !conformingToolName(sent) {
		t.Fatalf("a non-conforming tool name must be sanitized to a conforming form, got %q", sent)
	}
	if cc.toolMap.restore(sent) != orig {
		t.Fatalf("restore must recover the original name, got %q", cc.toolMap.restore(sent))
	}
	// the response tool_use carries the sanitized name; the converter restores it.
	sse := `data: {"type":"response.output_item.added","item":{"id":"f1","type":"function_call","call_id":"c1","name":"` + sent + `"}}` + "\n\n" +
		`data: {"type":"response.output_item.done","item":{"id":"f1","type":"function_call"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
	sink := &recSink{}
	if err := newStreamConverter(sink, cc).Convert(strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	var name any
	for i, ev := range sink.events {
		if ev != "content_block_start" {
			continue
		}
		if cb, _ := sink.payloads[i].(map[string]any)["content_block"].(map[string]any); cb != nil && cb["type"] == "tool_use" {
			name = cb["name"]
		}
	}
	if name != orig {
		t.Fatalf("response tool_use name must be restored to %q, got %v", orig, name)
	}
}

func TestToolNames_ConformingYieldsNilMap(t *testing.T) {
	a := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"mcp__plugin__do_it","input_schema":{"type":"object"}}]}`)
	if cc := newConvCtx(a, ""); cc.toolMap != nil {
		t.Fatal("an all-conforming tool set must yield a nil toolMap (nothing rewritten)")
	}
}

// "foo.bar" (non-conforming) sanitizes to "foo_bar", which is ALSO a real conforming
// tool — the rewritten one must be suffixed so restore stays 1:1 and routes correctly.
func TestToolNames_NoCollisionWithConforming(t *testing.T) {
	a := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"foo.bar","input_schema":{"type":"object"}},{"name":"foo_bar","input_schema":{"type":"object"}}]}`)
	cc := newConvCtx(a, "")
	dotted := cc.toolMap.sanitize("foo.bar")
	if dotted == "foo_bar" {
		t.Fatalf("a sanitized name must not collide with a real conforming tool, got %q", dotted)
	}
	if cc.toolMap.restore("foo_bar") != "foo_bar" {
		t.Fatalf("the conforming tool must restore to itself, got %q", cc.toolMap.restore("foo_bar"))
	}
	if cc.toolMap.restore(dotted) != "foo.bar" {
		t.Fatalf("the sanitized name must restore to foo.bar, got %q", cc.toolMap.restore(dotted))
	}
}

// a forced tool_choice naming a non-conforming tool must carry the SAME sanitized name
// as the tool def, or the backend can't match it.
func TestToolChoice_ForcedToolNameSanitized(t *testing.T) {
	a := parseReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"mcp.do","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"mcp.do"}}`)
	cc := newConvCtx(a, "")
	r, _ := translateRequest(a, cc)
	defName := r.Tools[0].Name
	if defName == "mcp.do" {
		t.Fatalf("the non-conforming tool def name should have been sanitized, got %q", defName)
	}
	tcMap, _ := r.ToolChoice.(map[string]any)
	if tcMap == nil || tcMap["name"] != defName {
		t.Fatalf("forced tool_choice name %v must equal the sanitized tool def name %q", r.ToolChoice, defName)
	}
}

// The codex lane presents no key (cc.apiKey == ""), so the Responses converter
// installs NO redactor — its canonical upstream errors pass through verbatim, even a
// key-shaped token (contrast the openai-* redaction test). Pins that the per-key redact
// seam never mangles a codex error.
func TestCodexLane_CanonicalErrorNotRedacted(t *testing.T) {
	const token = "sk-proj-CANON123" // key-shaped; codex has no key so it must NOT be masked
	sse := `data: {"type":"response.failed","response":{"error":{"message":"backend said ` + token + `"}}}` + "\n\n"
	sink := &recSink{}
	if err := newStreamConverter(sink, ccTest("gpt-5.5")).Convert(strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	if msg := errMessage(sink); !strings.Contains(msg, token) {
		t.Fatalf("codex lane must not redact (no key presented); canonical error was mangled: %q", msg)
	}
}

// ---- item 7: openai-chat cached-token split --------------------------------

func TestChatUsage_CachedSplit(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":60}}}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	sink := &recSink{}
	if err := newChatStreamConverter(sink, ccTest("m")).Convert(strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}
	usage, _ := sink.payload("message_delta", 0)["usage"].(map[string]any)
	if usage["input_tokens"] != 40 || usage["cache_read_input_tokens"] != 60 {
		t.Fatalf("chat cached split: input=%v cache_read=%v, want 40/60", usage["input_tokens"], usage["cache_read_input_tokens"])
	}
}
