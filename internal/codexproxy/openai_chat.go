package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/redact"
)

// openaiChatUpstream speaks the OpenAI Chat Completions API
// (POST <upstream>/chat/completions) with a per-request Bearer key. It is the
// upstream for the openai-chat protocol and the only one with a hand-written
// translator + converter (the Responses path reuses codex's).
type openaiChatUpstream struct {
	http    *http.Client
	baseURL string
}

func newOpenAIChatUpstream(baseURL string) *openaiChatUpstream {
	return &openaiChatUpstream{http: &http.Client{Timeout: 0}, baseURL: baseURL}
}

// models is empty: an openai-* provider's model list comes from the real upstream
// models_endpoint (probed directly with the key), never from this daemon.
func (u *openaiChatUpstream) models() []string { return nil }

func (u *openaiChatUpstream) convert(body io.Reader, sink sseSink, model, apiKey string) error {
	c := newChatStreamConverter(sink, model)
	c.redact = func(s string) string { return redactKey(s, apiKey) }
	return c.Convert(body)
}

// call translates the Anthropic request to a Chat Completions request, sends it
// with Authorization: Bearer <apiKey>, and returns the streaming body. On a
// non-2xx the response body is redacted (an arbitrary endpoint may echo the key)
// before it becomes a classified *upstreamError.
func (u *openaiChatUpstream) call(ctx context.Context, areq *anthropicRequest, apiKey string) (io.ReadCloser, error) {
	creq, err := translateChatRequest(areq)
	if err != nil {
		return nil, &upstreamError{upBadRequest, http.StatusBadRequest, err.Error()}
	}
	body, _ := json.Marshal(creq)
	endpoint, err := url.JoinPath(u.baseURL, "chat", "completions")
	if err != nil {
		return nil, &upstreamError{upBadRequest, http.StatusBadRequest, "invalid upstream url"}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, &upstreamError{upTransient, http.StatusBadGateway, "openai upstream: " + redactKey(err.Error(), apiKey)}
	}
	if resp.StatusCode/100 == 2 {
		return resp.Body, nil
	}
	return nil, classifyOpenAI(resp, apiKey)
}

// classifyOpenAI maps a non-2xx Chat/Responses status to an upstreamError. Unlike
// the codex backend it has no Cloudflare path and no OAuth-refresh: 401/403 is a
// terminal auth failure, 429 a standard rate limit. The body is redacted first.
func classifyOpenAI(resp *http.Response, apiKey string) *upstreamError {
	body := redactKey(drain(resp), apiKey)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return &upstreamError{upQuota, http.StatusTooManyRequests, "openai rate limited: " + body}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &upstreamError{upAuth, resp.StatusCode, "openai auth rejected: " + body}
	case resp.StatusCode/100 == 5:
		return &upstreamError{upTransient, resp.StatusCode, fmt.Sprintf("openai upstream http %d", resp.StatusCode)}
	default:
		return &upstreamError{upBadRequest, resp.StatusCode, fmt.Sprintf("openai upstream http %d: %s", resp.StatusCode, body)}
	}
}

// redactKey masks the exact presented key AND any key-shaped token, so an upstream
// that echoes the Authorization/x-api-key bytes can never reach the client.
func redactKey(s, apiKey string) string {
	if apiKey != "" {
		s = strings.ReplaceAll(s, apiKey, "sk-[REDACTED]")
	}
	return redact.MaskKeyLikeString(s)
}

// ---- Anthropic -> Chat Completions request ---------------------------------

type chatRequest struct {
	Model             string         `json:"model"`
	Messages          []any          `json:"messages"`
	MaxTokens         int            `json:"max_tokens,omitempty"`
	Tools             []chatTool     `json:"tools,omitempty"`
	ToolChoice        any            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Stream            bool           `json:"stream"`
	StreamOptions     *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// translateChatRequest maps an Anthropic Messages request to a Chat Completions
// request. thinking is dropped (most compatible endpoints don't accept it); the
// usage chunk is requested via stream_options.include_usage.
func translateChatRequest(a *anthropicRequest) (*chatRequest, error) {
	r := &chatRequest{
		Model:         a.Model,
		MaxTokens:     a.MaxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if sys := systemText(a.System); sys != "" {
		r.Messages = append(r.Messages, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range a.Messages {
		items, err := translateChatMessage(m)
		if err != nil {
			return nil, err
		}
		r.Messages = append(r.Messages, items...)
	}
	for _, t := range a.Tools {
		r.Tools = append(r.Tools, chatTool{Type: "function", Function: chatFunction{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
	}
	r.ToolChoice, r.ParallelToolCalls = translateChatToolChoice(a.ToolChoice)
	return r, nil
}

// translateChatMessage maps one Anthropic message to one or more Chat messages.
// tool_result blocks become role:"tool" messages emitted FIRST (Chat requires a
// tool result to immediately follow the assistant turn that called it); text +
// images form the message content; assistant tool_use blocks become tool_calls.
func translateChatMessage(m anthropicMessage) ([]any, error) {
	if s := stringContent(m.Content); s != nil {
		return []any{map[string]any{"role": m.Role, "content": *s}}, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("message content: %w", err)
	}

	var (
		toolMsgs  []any
		parts     []map[string]any
		hasImage  bool
		text      strings.Builder
		toolCalls []map[string]any
	)
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			text.WriteString(b.Text)
		case "image":
			if u := imageDataURL(b.Source); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				hasImage = true
			}
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": string(rawOrEmptyObject(b.Input))},
			})
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role": "tool", "tool_call_id": b.ToolUseID, "content": toolResultText(b.Content),
			})
		case "thinking":
			// dropped — Chat Completions has no thinking input channel.
		}
	}

	out := toolMsgs // tool results precede any trailing text for this turn
	var content any
	switch {
	case hasImage:
		content = parts // array form carries text parts + image parts
	case text.Len() > 0:
		content = text.String()
	}
	if m.Role == "assistant" {
		if content == nil {
			content = "" // assistant turn with only tool_calls
		}
		msg := map[string]any{"role": "assistant", "content": content}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
	} else if content != nil {
		out = append(out, map[string]any{"role": m.Role, "content": content})
	}
	return out, nil
}

// stringContent returns the message content as a *string when it is a plain
// string, else nil (it is a block array).
func stringContent(raw json.RawMessage) *string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return &s
	}
	return nil
}

// translateChatToolChoice maps Anthropic tool_choice to the Chat form plus the
// parallel-tool-calls flag (set only when explicitly disabled). auto->auto,
// any->required, {type:tool,name}->{type:function,function:{name}}, none->none.
func translateChatToolChoice(raw json.RawMessage) (choice any, parallel *bool) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tc struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil, nil
	}
	if tc.DisableParallelToolUse {
		f := false
		parallel = &f
	}
	switch tc.Type {
	case "any":
		return "required", parallel
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}, parallel
	case "none":
		return "none", parallel
	default:
		return "auto", parallel
	}
}
