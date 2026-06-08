package codexproxy

import (
	"encoding/json"
	"fmt"
)

// anthropicRequest is the subset of the Anthropic Messages request that the claude
// CLI sends; unknown fields are ignored.
type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     json.RawMessage    `json:"system"` // string OR []{type:text,text}
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools"`
	ToolChoice json.RawMessage    `json:"tool_choice"` // {type:auto|any|tool,name,disable_parallel_tool_use}
	Stream     bool               `json:"stream"`
	Thinking   *anthropicThinking `json:"thinking"`
	Metadata   struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OR []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool_use name
	Input     json.RawMessage `json:"input"`       // tool_use input
	ToolUseID string          `json:"tool_use_id"` // tool_result -> the tool_use id
	Content   json.RawMessage `json:"content"`     // tool_result content (string OR []block)
	Source    *imageSource    `json:"source"`      // image
	Thinking  string          `json:"thinking"`    // thinking block text
	Signature string          `json:"signature"`   // thinking block signature = the replayed encrypted_content
}

type imageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// responsesRequest is the OpenAI Responses body sent to the ChatGPT backend.
type responsesRequest struct {
	Model             string          `json:"model"`
	Instructions      string          `json:"instructions,omitempty"`
	Input             []any           `json:"input"`
	Tools             []responsesTool `json:"tools,omitempty"`
	ToolChoice        any             `json:"tool_choice,omitempty"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Reasoning         *reasoningOpt   `json:"reasoning,omitempty"`
	Include           []string        `json:"include,omitempty"`
	Store             bool            `json:"store"`
	Stream            bool            `json:"stream"`
	PromptCacheKey    string          `json:"prompt_cache_key,omitempty"`
	// MaxOutputTokens is set only by the openai-responses upstream (a billed
	// account honors the cap); the codex backend 400s on it, so translateRequest
	// leaves it 0 and omitempty drops it.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

type reasoningOpt struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// translateRequest maps an Anthropic Messages request to a Responses request.
// max_tokens and temperature are intentionally omitted (the ChatGPT backend 400s
// on them). store is forced false. The claude session id inside metadata.user_id
// becomes prompt_cache_key so the backend's prompt cache follows the conversation.
func translateRequest(a *anthropicRequest) (*responsesRequest, error) {
	choice, parallel := translateToolChoice(a.ToolChoice)
	r := &responsesRequest{
		Model:             a.Model,
		Instructions:      systemText(a.System),
		Store:             false,
		Stream:            true,
		ToolChoice:        choice,
		ParallelToolCalls: parallel,
		PromptCacheKey:    a.Metadata.UserID,
	}
	for _, m := range a.Messages {
		items, err := translateMessage(m)
		if err != nil {
			return nil, err
		}
		r.Input = append(r.Input, items...)
	}
	for _, t := range a.Tools {
		r.Tools = append(r.Tools, responsesTool{
			Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		})
	}
	// "enabled" carries a budget; "adaptive" (claude's default-on mode) has none
	// and lands in the low bucket. Either way the model reasons — requesting the
	// summary + encrypted_content keeps that reasoning replayable across turns.
	if a.Thinking != nil && (a.Thinking.Type == "enabled" || a.Thinking.Type == "adaptive") {
		r.Reasoning = &reasoningOpt{Effort: effortBucket(a.Thinking.BudgetTokens), Summary: "auto"}
		r.Include = []string{"reasoning.encrypted_content"}
	}
	return r, nil
}

// systemText flattens the Anthropic system field (string OR []{type:text,text})
// to the Responses instructions string.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		out := ""
		for _, b := range blocks {
			if b.Type == "text" {
				if out != "" {
					out += "\n"
				}
				out += b.Text
			}
		}
		return out
	}
	return ""
}

// translateToolChoice maps Anthropic tool_choice to the Responses form plus the
// parallel-tool-calls flag. auto->auto, any->required, {type:tool,name}->
// {type:function,name}; nil/absent -> auto. It is NOT hard-forced to auto (that
// would break forced-tool / StructuredOutput paths). disable_parallel_tool_use
// flips parallel_tool_calls off.
func translateToolChoice(raw json.RawMessage) (choice any, parallel bool) {
	if len(raw) == 0 {
		return "auto", true
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return "auto", true
	}
	parallel = !tc.DisableParallelToolUse
	switch tc.Type {
	case "any":
		return "required", parallel
	case "tool":
		return map[string]any{"type": "function", "name": tc.Name}, parallel
	default:
		return "auto", parallel
	}
}

// effortBucket maps an Anthropic thinking budget to a Responses reasoning effort.
func effortBucket(budget int) string {
	switch {
	case budget <= 0:
		return "low"
	case budget <= 8192:
		return "medium"
	default:
		return "high"
	}
}

// translateMessage maps one Anthropic message to one or more Responses input items.
func translateMessage(m anthropicMessage) ([]any, error) {
	// claude -p sends mid-conversation system-role messages (e.g. the skills
	// listing); the codex backend rejects role "system" in input, so they ride
	// as "developer" (same authority tier in the Responses role hierarchy).
	role := m.Role
	if role == "system" {
		role = "developer"
	}
	// string content -> a single text message
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []any{textMessage(role, s)}, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("message content: %w", err)
	}
	var items []any
	var textParts []map[string]any
	flushText := func() {
		if len(textParts) > 0 {
			items = append(items, map[string]any{"type": "message", "role": role, "content": textParts})
			textParts = nil
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, map[string]any{"type": textKind(role), "text": b.Text})
		case "image":
			if url := imageDataURL(b.Source); url != "" {
				textParts = append(textParts, map[string]any{"type": "input_image", "image_url": url})
			}
		case "thinking":
			// Reasoning continuity: the block's signature carries the Responses
			// encrypted_content the stream converter emitted; replay it as a
			// reasoning item WITHOUT an id (store:false rejects replayed ids).
			// A signature-less thinking block has nothing replayable — skip it.
			if b.Signature == "" {
				continue
			}
			flushText()
			summary := []map[string]any{}
			if b.Thinking != "" {
				summary = append(summary, map[string]any{"type": "summary_text", "text": b.Thinking})
			}
			items = append(items, map[string]any{
				"type": "reasoning", "summary": summary, "encrypted_content": b.Signature,
			})
		case "tool_use":
			flushText()
			items = append(items, map[string]any{
				"type": "function_call", "call_id": b.ID, "name": b.Name,
				"arguments": string(rawOrEmptyObject(b.Input)),
			})
		case "tool_result":
			flushText()
			items = append(items, map[string]any{
				"type": "function_call_output", "call_id": b.ToolUseID, "output": toolResultText(b.Content),
			})
		}
	}
	flushText()
	return items, nil
}

func textMessage(role, text string) map[string]any {
	return map[string]any{"type": "message", "role": role,
		"content": []map[string]any{{"type": textKind(role), "text": text}}}
}

// textKind picks the Responses content type for a role's plain text.
func textKind(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

func imageDataURL(src *imageSource) string {
	if src == nil {
		return ""
	}
	if src.Type == "url" && src.URL != "" {
		return src.URL
	}
	if src.Type == "base64" && src.Data != "" {
		return fmt.Sprintf("data:%s;base64,%s", src.MediaType, src.Data)
	}
	return ""
}

func rawOrEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// toolResultText flattens an Anthropic tool_result content (string OR []block) to
// the plain string the Responses function_call_output carries.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		out := ""
		for _, b := range blocks {
			if b.Type == "text" {
				out += b.Text
			}
		}
		return out
	}
	return string(raw)
}
