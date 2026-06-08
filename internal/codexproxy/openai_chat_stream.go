package codexproxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/redact"
)

// chatStreamConverter turns a Chat Completions SSE stream (chat.completion.chunk)
// into a valid Anthropic Messages SSE stream, with the same grammar + idempotent
// guards as the Responses converter: a clean stream is message_start -> per-block
// start/delta/stop -> message_delta -> message_stop; a mid-stream error chunk or a
// transport read error ends on an Anthropic error event with open blocks closed,
// never a clean message_stop.
type chatStreamConverter struct {
	out     sseSink
	model   string
	toolMap *toolNameMap // restores a sanitized tool name onto the response tool_use block
	// redact masks a streaming error message before it reaches the client (set by
	// the openai-chat upstream to scrub an echoed key, exact + pattern). When nil
	// it falls back to the generic key-pattern mask.
	redact func(string) string

	started   bool
	failed    bool
	nextIndex int

	textOpen  bool
	textIndex int
	tools     map[int]int  // chat tool_calls index -> Anthropic block index
	toolOpen  map[int]bool // chat tool_calls index -> open

	stopReason      string
	inTokens        int
	outTokens       int
	cacheReadTokens int
}

func newChatStreamConverter(out sseSink, cc *convCtx) *chatStreamConverter {
	c := &chatStreamConverter{
		out: out, model: cc.model, toolMap: cc.toolMap, textIndex: -1,
		tools: map[int]int{}, toolOpen: map[int]bool{}, stopReason: "end_turn",
	}
	if cc.apiKey != "" {
		c.redact = func(s string) string { return redactKey(s, cc.apiKey) }
	}
	return c
}

type chatChunk struct {
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage"`
	Error   *chatError   `json:"error"`
}

type chatChoice struct {
	Delta        chatDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type chatDelta struct {
	Role             string              `json:"role"`
	Content          *string             `json:"content"`
	ReasoningContent *string             `json:"reasoning_content"`
	Refusal          *string             `json:"refusal"`
	ToolCalls        []chatToolCallDelta `json:"tool_calls"`
}

type chatToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type chatError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

// Convert reads the Chat Completions SSE body to completion, emitting Anthropic
// SSE. A clean end-of-stream closes open blocks and emits message_delta +
// message_stop; an error chunk or a transport read error emits an Anthropic error
// event instead, so a failure is never handed to Claude as a successful answer.
func (c *chatStreamConverter) Convert(body io.Reader) error {
	if err := c.ensureStarted(); err != nil {
		return err
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var ev chatChunk
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if ev.Error != nil {
			return c.emitError("openai upstream: " + ev.Error.Message)
		}
		if err := c.handle(&ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		_ = c.emitError("openai upstream stream read error")
		return err
	}
	c.finish()
	return nil
}

func (c *chatStreamConverter) handle(ev *chatChunk) error {
	if ev.Usage != nil {
		if ev.Usage.PromptTokens > 0 {
			// OpenAI prompt_tokens INCLUDES the cached prefix; Anthropic's input_tokens
			// excludes cache reads, so split the cached count into cache_read_input_tokens.
			cached := ev.Usage.PromptTokensDetails.CachedTokens
			if cached < 0 || cached > ev.Usage.PromptTokens {
				cached = 0
			}
			c.inTokens = ev.Usage.PromptTokens - cached
			c.cacheReadTokens = cached
		}
		if ev.Usage.CompletionTokens > 0 {
			c.outTokens = ev.Usage.CompletionTokens
		}
	}
	if len(ev.Choices) == 0 {
		return nil // a usage-only chunk carries no choices
	}
	ch := ev.Choices[0]
	// content text and refusal both surface as visible text; reasoning_content is a
	// separate channel and is deliberately dropped (never concatenated into text).
	if txt := chatVisibleText(ch.Delta); txt != "" {
		if err := c.textDelta(txt); err != nil {
			return err
		}
	}
	for _, tc := range ch.Delta.ToolCalls {
		if err := c.toolDelta(tc); err != nil {
			return err
		}
	}
	if ch.FinishReason != nil {
		c.applyFinish(*ch.FinishReason)
	}
	return nil
}

// chatVisibleText returns the user-visible text of a delta: content, or a refusal
// (surfaced, not silently dropped). A nil/empty content opens no block.
func chatVisibleText(d chatDelta) string {
	if d.Content != nil {
		return *d.Content
	}
	if d.Refusal != nil {
		return *d.Refusal
	}
	return ""
}

func (c *chatStreamConverter) textDelta(txt string) error {
	if !c.textOpen {
		c.textIndex = c.nextIndex
		c.nextIndex++
		c.textOpen = true
		if err := c.out.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": c.textIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
	}
	return c.out.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": c.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": txt},
	})
}

// toolDelta opens a tool_use block on the first fragment for a chat tool index
// (which carries the id + name) and streams later argument fragments as
// input_json_delta — never buffered-and-parsed.
func (c *chatStreamConverter) toolDelta(tc chatToolCallDelta) error {
	idx, ok := c.tools[tc.Index]
	if !ok {
		idx = c.nextIndex
		c.nextIndex++
		c.tools[tc.Index] = idx
		c.toolOpen[tc.Index] = true
		c.stopReason = "tool_use"
		if err := c.out.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": c.toolMap.restore(tc.Function.Name), "input": map[string]any{}},
		}); err != nil {
			return err
		}
	}
	if tc.Function.Arguments == "" {
		return nil
	}
	return c.out.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
	})
}

// applyFinish maps a Chat finish_reason to an Anthropic stop_reason. stop /
// content_filter / absent leave the current reason (end_turn, or tool_use if a
// tool block opened); length and tool_calls override explicitly.
func (c *chatStreamConverter) applyFinish(reason string) {
	switch reason {
	case "length":
		c.stopReason = "max_tokens"
	case "tool_calls":
		c.stopReason = "tool_use"
	}
}

func (c *chatStreamConverter) ensureStarted() error {
	if c.started {
		return nil
	}
	c.started = true
	return c.out.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_codexproxy", "type": "message", "role": "assistant", "model": c.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// finish closes any still-open block and emits message_delta + message_stop.
// Guarded by started (a failure before message_start emits nothing) and failed
// (an errored stream ends on its error event, not a clean stop).
func (c *chatStreamConverter) finish() {
	if !c.started || c.failed {
		return
	}
	_ = c.closeAllOpen()
	_ = c.out.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": c.stopReason, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":            c.inTokens,
			"output_tokens":           c.outTokens,
			"cache_read_input_tokens": c.cacheReadTokens,
		},
	})
	_ = c.out.event("message_stop", map[string]any{"type": "message_stop"})
}

// emitError closes any open block and emits a redacted Anthropic error event,
// marking the stream failed so finish() skips the clean message_delta/stop.
func (c *chatStreamConverter) emitError(msg string) error {
	if err := c.closeAllOpen(); err != nil {
		return err
	}
	c.failed = true
	if c.redact != nil {
		msg = c.redact(msg)
	} else {
		msg = redact.MaskKeyLikeString(msg)
	}
	return c.out.event("error", map[string]any{
		"type": "error", "error": map[string]any{"type": "api_error", "message": msg},
	})
}

func (c *chatStreamConverter) closeAllOpen() error {
	if c.textOpen {
		c.textOpen = false
		if err := c.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.textIndex}); err != nil {
			return err
		}
	}
	for chatIdx, idx := range c.tools {
		if c.toolOpen[chatIdx] {
			c.toolOpen[chatIdx] = false
			if err := c.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}); err != nil {
				return err
			}
		}
	}
	return nil
}
