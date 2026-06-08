package codexproxy

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// sseSink writes Anthropic SSE events and flushes each one.
type sseSink interface {
	event(name string, data any) error
}

// blockKind is the Anthropic content-block type a Responses output item maps to.
type blockKind int

const (
	blockText blockKind = iota
	blockToolUse
	blockThinking
)

type blockState struct {
	index int
	kind  blockKind
	open  bool
}

// streamConverter turns a Responses SSE stream into a valid Anthropic Messages SSE
// stream. A clean stream is message_start -> per-block start/delta/stop ->
// message_delta -> message_stop, with idempotent guards so a block is never left
// open (Claude Code hangs on a dangling block). A failed upstream event or a
// transport read error instead ends on an Anthropic error event (open blocks
// closed first), never a clean message_stop.
type streamConverter struct {
	out   sseSink
	model string

	started         bool
	failed          bool // an upstream failure was surfaced as an Anthropic error event
	nextIndex       int
	blocks          map[string]*blockState // keyed by Responses item id (or output_index)
	stopReason      string
	inTokens        int
	outTokens       int
	cacheReadTokens int
}

func newStreamConverter(out sseSink, model string) *streamConverter {
	return &streamConverter{out: out, model: model, blocks: map[string]*blockState{}, stopReason: "end_turn"}
}

// responsesEvent is the union of Responses streaming event fields we read.
type responsesEvent struct {
	Type        string          `json:"type"`
	OutputIndex *int            `json:"output_index"`
	ItemID      string          `json:"item_id"`
	Item        *responsesItem  `json:"item"`
	Delta       string          `json:"delta"`
	Response    json.RawMessage `json:"response"`
	Error       *responsesError `json:"error"`
}

type responsesItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"` // message | function_call | reasoning
	Name             string `json:"name"`
	CallID           string `json:"call_id"`
	EncryptedContent string `json:"encrypted_content"`
}

type responsesError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// Convert reads the upstream SSE body to completion, emitting Anthropic SSE. A
// clean end-of-stream — including a body that stopped early without a
// done/completed event but WITH no read error — closes open blocks and emits
// message_delta + message_stop. A response.failed/error event (see failStream) or
// a transport read error mid-stream instead emits an Anthropic error event, so a
// transport failure is never handed to Claude as a successful answer.
func (c *streamConverter) Convert(body io.Reader) error {
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
		var ev responsesEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		if err := c.handle(&ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		_ = c.emitError("codex upstream stream read error")
		return err
	}
	c.finish()
	return nil
}

func (c *streamConverter) key(ev *responsesEvent) string {
	if ev.ItemID != "" {
		return ev.ItemID
	}
	if ev.Item != nil && ev.Item.ID != "" {
		return ev.Item.ID
	}
	if ev.OutputIndex != nil {
		return "idx-" + strconv.Itoa(*ev.OutputIndex)
	}
	return ""
}

func (c *streamConverter) handle(ev *responsesEvent) error {
	switch {
	case ev.Type == "response.output_item.added" && ev.Item != nil:
		return c.openBlock(ev)
	case ev.Type == "response.output_text.delta":
		return c.blockDelta(ev, "text_delta", "text", ev.Delta)
	case ev.Type == "response.function_call_arguments.delta":
		return c.blockDelta(ev, "input_json_delta", "partial_json", ev.Delta)
	case ev.Type == "response.reasoning_summary_text.delta":
		return c.blockDelta(ev, "thinking_delta", "thinking", ev.Delta)
	case ev.Type == "response.output_item.done":
		return c.finishItem(ev)
	case ev.Type == "response.completed" || ev.Type == "response.done" || ev.Type == "response.incomplete":
		c.readUsageAndStop(ev.Response)
	case ev.Type == "response.failed" || ev.Type == "error":
		return c.failStream(ev)
	}
	return nil
}

// finishItem closes a finished output item's block. A reasoning item's
// encrypted_content is emitted first as the thinking block's signature_delta —
// claude hands it back as the block's signature on the next turn, which is how
// reasoning context survives a stateless (store:false) conversation.
func (c *streamConverter) finishItem(ev *responsesEvent) error {
	key := c.key(ev)
	if bs := c.blocks[key]; bs != nil && bs.open && bs.kind == blockThinking &&
		ev.Item != nil && ev.Item.EncryptedContent != "" {
		if err := c.out.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": bs.index,
			"delta": map[string]any{"type": "signature_delta", "signature": ev.Item.EncryptedContent},
		}); err != nil {
			return err
		}
	}
	return c.closeBlock(key)
}

// failStream surfaces an upstream mid-stream failure as an Anthropic error
// event: open blocks are closed, the error event is emitted, and finish()
// skips the normal message_delta/message_stop (the turn did not complete — a
// clean stop here would make a failed answer look like a short success).
func (c *streamConverter) failStream(ev *responsesEvent) error {
	msg := "codex upstream stream failed"
	rerr := ev.Error
	if rerr == nil && len(ev.Response) > 0 {
		var r struct {
			Error *responsesError `json:"error"`
		}
		if json.Unmarshal(ev.Response, &r) == nil {
			rerr = r.Error
		}
	}
	if rerr != nil && rerr.Message != "" {
		msg = "codex upstream: " + rerr.Message
	}
	return c.emitError(msg)
}

// emitError closes any open block and emits an Anthropic error event, marking the
// stream failed so finish() skips the normal message_delta/message_stop.
func (c *streamConverter) emitError(msg string) error {
	if err := c.closeAllOpen(); err != nil {
		return err
	}
	c.failed = true
	return c.out.event("error", map[string]any{
		"type": "error", "error": map[string]any{"type": "api_error", "message": msg},
	})
}

func (c *streamConverter) openBlock(ev *responsesEvent) error {
	key := c.key(ev)
	if key == "" || c.blocks[key] != nil {
		return nil
	}
	var (
		kind blockKind
		cb   map[string]any
	)
	switch ev.Item.Type {
	case "function_call":
		kind = blockToolUse
		c.stopReason = "tool_use"
		cb = map[string]any{"type": "tool_use", "id": ev.Item.CallID, "name": ev.Item.Name, "input": map[string]any{}}
	case "reasoning":
		kind = blockThinking
		cb = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "message":
		kind = blockText
		cb = map[string]any{"type": "text", "text": ""}
	default:
		return nil // unknown item type: no block
	}
	bs := &blockState{index: c.nextIndex, kind: kind, open: true}
	c.nextIndex++
	c.blocks[key] = bs
	return c.out.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": bs.index, "content_block": cb,
	})
}

// blockDelta routes a Responses delta to its Anthropic block, lazily opening a text
// block if the upstream emitted text deltas without an explicit item.added (defensive).
func (c *streamConverter) blockDelta(ev *responsesEvent, deltaType, field, val string) error {
	key := c.key(ev)
	bs := c.blocks[key]
	if bs == nil {
		bs = &blockState{index: c.nextIndex, kind: blockText, open: true}
		c.nextIndex++
		c.blocks[key] = bs
		if err := c.out.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": bs.index, "content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return err
		}
	}
	if val == "" {
		return nil
	}
	return c.out.event("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": bs.index,
		"delta": map[string]any{"type": deltaType, field: val},
	})
}

func (c *streamConverter) closeBlock(key string) error {
	bs := c.blocks[key]
	if bs == nil || !bs.open {
		return nil
	}
	bs.open = false
	return c.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": bs.index})
}

func (c *streamConverter) ensureStarted() error {
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

// finish closes any still-open block and emits message_delta + message_stop. Safe to
// call once; guarded by started so a failure before message_start emits nothing, and
// by failed so an errored stream ends on its error event, not a clean stop.
func (c *streamConverter) finish() {
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

// closeAllOpen emits content_block_stop for every still-open block.
func (c *streamConverter) closeAllOpen() error {
	for _, bs := range c.blocks {
		if bs.open {
			bs.open = false
			if err := c.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": bs.index}); err != nil {
				return err
			}
		}
	}
	return nil
}

// readUsageAndStop pulls usage + a terminal status from a response.completed/
// done/incomplete envelope and maps the status to an Anthropic stop_reason
// (unless a tool_use block already set it). OpenAI input_tokens INCLUDES the
// cached prefix while Anthropic's excludes cache reads, so the cached count is
// subtracted out and reported as cache_read_input_tokens.
func (c *streamConverter) readUsageAndStop(raw json.RawMessage) {
	var r struct {
		Status string `json:"status"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r)
	}
	if r.Usage.InputTokens > 0 {
		cached := r.Usage.InputTokensDetails.CachedTokens
		if cached < 0 || cached > r.Usage.InputTokens {
			cached = 0
		}
		c.inTokens = r.Usage.InputTokens - cached
		c.cacheReadTokens = cached
	}
	if r.Usage.OutputTokens > 0 {
		c.outTokens = r.Usage.OutputTokens
	}
	if c.stopReason == "tool_use" {
		return
	}
	switch r.Status {
	case "incomplete":
		c.stopReason = "max_tokens"
	default:
		c.stopReason = "end_turn"
	}
}
