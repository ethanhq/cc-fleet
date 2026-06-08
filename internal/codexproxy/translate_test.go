package codexproxy

import (
	"encoding/json"
	"testing"
)

func TestSystemText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", ``, ""},
		{"string", `"hello"`, "hello"},
		{"blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := systemText(json.RawMessage(c.raw)); got != c.want {
				t.Fatalf("systemText(%s)=%q want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestTranslateToolChoice(t *testing.T) {
	cases := []struct {
		raw          string
		want         any
		wantParallel bool
	}{
		{``, "auto", true},
		{`{"type":"auto"}`, "auto", true},
		{`{"type":"any"}`, "required", true},
		{`{"type":"tool","name":"grep"}`, map[string]any{"type": "function", "name": "grep"}, true},
		{`{"type":"auto","disable_parallel_tool_use":true}`, "auto", false},
	}
	for _, c := range cases {
		got, parallel := translateToolChoice(json.RawMessage(c.raw))
		if parallel != c.wantParallel {
			t.Fatalf("toolChoice(%s) parallel=%v want %v", c.raw, parallel, c.wantParallel)
		}
		if m, ok := c.want.(map[string]any); ok {
			gm, ok := got.(map[string]any)
			if !ok || gm["type"] != m["type"] || gm["name"] != m["name"] {
				t.Fatalf("toolChoice(%s)=%v want %v", c.raw, got, c.want)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("toolChoice(%s)=%v want %v", c.raw, got, c.want)
		}
	}
}

func TestEffortBucket(t *testing.T) {
	for _, c := range []struct {
		budget int
		want   string
	}{{0, "low"}, {1024, "medium"}, {8192, "medium"}, {32000, "high"}} {
		if got := effortBucket(c.budget); got != c.want {
			t.Fatalf("effortBucket(%d)=%s want %s", c.budget, got, c.want)
		}
	}
}

func TestTranslateRequest_OmitsForbiddenFieldsAndForcesStore(t *testing.T) {
	a := &anthropicRequest{
		Model:     "gpt-5.5",
		MaxTokens: 999,
		System:    json.RawMessage(`"sys"`),
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	a.Metadata.UserID = "user_abc_session_123"
	r, err := translateRequest(a)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	s := string(b)
	if contains(s, "max_output_tokens") || contains(s, "max_tokens") || contains(s, "temperature") {
		t.Fatalf("request leaked a forbidden field: %s", s)
	}
	if r.Store {
		t.Fatal("store must be false")
	}
	if r.Instructions != "sys" {
		t.Fatalf("instructions=%q", r.Instructions)
	}
	if len(r.Input) != 1 {
		t.Fatalf("want 1 input item, got %d", len(r.Input))
	}
	if r.PromptCacheKey != "user_abc_session_123" {
		t.Fatalf("prompt_cache_key=%q (want the metadata user_id)", r.PromptCacheKey)
	}
}

func TestTranslateMessage_SystemRoleRidesAsDeveloper(t *testing.T) {
	// claude -p sends a system-role skills message; the codex backend rejects
	// role "system" in input, so it must ride as "developer".
	items, err := translateMessage(anthropicMessage{
		Role:    "system",
		Content: json.RawMessage(`"The following skills are available"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	m := items[0].(map[string]any)
	if m["role"] != "developer" {
		t.Fatalf("system message role = %v, want developer", m["role"])
	}
	blocks, _ := m["content"].([]map[string]any)
	if len(blocks) != 1 || blocks[0]["type"] != "input_text" {
		t.Fatalf("developer content = %v", m["content"])
	}
}

func TestTranslateMessage_ThinkingReplaysEncryptedReasoning(t *testing.T) {
	items, err := translateMessage(anthropicMessage{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"thinking","thinking":"plan...","signature":"gAAAAA-enc"},
			{"type":"thinking","thinking":"no signature -> skipped"},
			{"type":"text","text":"answer"}
		]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("want reasoning + message, got %d items: %v", len(items), items)
	}
	r := items[0].(map[string]any)
	if r["type"] != "reasoning" || r["encrypted_content"] != "gAAAAA-enc" {
		t.Fatalf("reasoning item = %v", r)
	}
	if _, hasID := r["id"]; hasID {
		t.Fatal("replayed reasoning item must not carry an id (store:false rejects it)")
	}
	sum := r["summary"].([]map[string]any)
	if len(sum) != 1 || sum[0]["text"] != "plan..." {
		t.Fatalf("summary = %v", sum)
	}
}

func TestTranslateMessage_ToolUseAndResult(t *testing.T) {
	// assistant tool_use -> function_call
	items, err := translateMessage(anthropicMessage{
		Role:    "assistant",
		Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"grep","input":{"q":"x"}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	m := items[0].(map[string]any)
	if m["type"] != "function_call" || m["call_id"] != "toolu_1" || m["name"] != "grep" {
		t.Fatalf("bad function_call item: %v", m)
	}
	if m["arguments"] != `{"q":"x"}` {
		t.Fatalf("arguments=%v", m["arguments"])
	}
	// user tool_result -> function_call_output
	items, _ = translateMessage(anthropicMessage{
		Role:    "user",
		Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"42"}]`),
	})
	m = items[0].(map[string]any)
	if m["type"] != "function_call_output" || m["call_id"] != "toolu_1" || m["output"] != "42" {
		t.Fatalf("bad function_call_output item: %v", m)
	}
}

func TestImageDataURL(t *testing.T) {
	if got := imageDataURL(&imageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}); got != "data:image/png;base64,AAAA" {
		t.Fatalf("base64 image url=%q", got)
	}
	if got := imageDataURL(&imageSource{Type: "url", URL: "https://x/y.png"}); got != "https://x/y.png" {
		t.Fatalf("url image=%q", got)
	}
	if got := imageDataURL(nil); got != "" {
		t.Fatalf("nil image=%q", got)
	}
}

// contains is a tiny helper local to tests.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
