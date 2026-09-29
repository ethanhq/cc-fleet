package codexproxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
)

// messageAssembler is the sseSink for a non-streaming (stream:false) request: it
// folds the converter's Anthropic SSE events back into one Anthropic Message.
// Claude Code falls back to a non-streaming request after a broken stream, so the
// reply must be a JSON Message (or a JSON error with a matching status), not SSE.
type messageAssembler struct {
	id, model    string
	usage        map[string]any
	stopReason   any
	stopSequence any
	blocks       map[int]*asmBlock
	stopped      bool // message_stop seen

	upErr    bool // an upstream error event was seen
	upErrMsg string
	asmErr   error // first block that could not be assembled
}

type asmBlock struct {
	kind      string // text | tool_use | thinking
	id, name  string
	text      string // text or thinking
	signature string
	json      string // accumulated tool_use input
	input     json.RawMessage
}

func newMessageAssembler() *messageAssembler {
	return &messageAssembler{usage: map[string]any{}, blocks: map[int]*asmBlock{}}
}

// asmEvent is the union of the Anthropic SSE event fields the assembler reads.
type asmEvent struct {
	Message *struct {
		ID    string         `json:"id"`
		Model string         `json:"model"`
		Usage map[string]any `json:"usage"`
	} `json:"message"`
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		PartialJSON  string `json:"partial_json"`
		Thinking     string `json:"thinking"`
		Signature    string `json:"signature"`
		StopReason   any    `json:"stop_reason"`
		StopSequence any    `json:"stop_sequence"`
	} `json:"delta"`
	Usage map[string]any `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// event never returns an error: a converter's emitError first closes open blocks
// (content_block_stop) and only then emits the error event, so failing on a
// half-written tool input there would drop the upstream error that explains it.
// The first assembly failure is kept in asmErr instead.
func (a *messageAssembler) event(name string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		a.keepErr(err)
		return nil
	}
	var ev asmEvent
	if err := json.Unmarshal(b, &ev); err != nil {
		a.keepErr(err)
		return nil
	}
	switch name {
	case "message_start":
		if ev.Message != nil {
			a.id, a.model = ev.Message.ID, ev.Message.Model
			mergeUsage(a.usage, ev.Message.Usage)
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			a.blocks[ev.Index] = &asmBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, input: json.RawMessage("{}")}
		}
	case "content_block_delta":
		blk := a.blocks[ev.Index]
		if blk == nil || ev.Delta == nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			blk.text += ev.Delta.Text
		case "input_json_delta":
			blk.json += ev.Delta.PartialJSON
		case "thinking_delta":
			blk.text += ev.Delta.Thinking
		case "signature_delta":
			blk.signature = ev.Delta.Signature
		}
	case "content_block_stop":
		// An empty input stays {}.
		if blk := a.blocks[ev.Index]; blk != nil && blk.kind == "tool_use" && blk.json != "" {
			var obj map[string]json.RawMessage
			// JSON null unmarshals into a nil map without error.
			if err := json.Unmarshal([]byte(blk.json), &obj); err != nil || obj == nil {
				a.keepErr(errors.New("upstream tool call arguments are not a valid JSON object"))
			} else {
				blk.input = json.RawMessage(blk.json)
			}
		}
	case "message_delta":
		if ev.Delta != nil {
			a.stopReason, a.stopSequence = ev.Delta.StopReason, ev.Delta.StopSequence
		}
		mergeUsage(a.usage, ev.Usage)
	case "message_stop":
		a.stopped = true
	case "error":
		if ev.Error != nil && !a.upErr {
			a.upErr, a.upErrMsg = true, ev.Error.Message
		}
	}
	return nil
}

func (a *messageAssembler) keepErr(err error) {
	if a.asmErr == nil {
		a.asmErr = err
	}
}

func mergeUsage(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

// write sends the response once convert has returned. An upstream error event
// wins (its status from upErrType), then an assembly or conversion failure (502),
// then a completed message (200); a stream that just stopped is a 502.
func (a *messageAssembler) write(w http.ResponseWriter, upErrType string, convErr error) {
	switch {
	case a.upErr:
		status, etype := statusFor(upErrType)
		writeAnthropicError(w, status, etype, a.upErrMsg)
	case a.asmErr != nil:
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "codexproxy could not assemble the upstream response: "+a.asmErr.Error())
	case convErr != nil:
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "codexproxy could not convert the upstream response")
	case a.stopped:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(a.message())
	default:
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream stream ended before the message completed")
	}
}

func (a *messageAssembler) message() map[string]any {
	idx := make([]int, 0, len(a.blocks))
	for i := range a.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	content := make([]any, 0, len(idx))
	for _, i := range idx {
		blk := a.blocks[i]
		switch blk.kind {
		case "text":
			content = append(content, map[string]any{"type": "text", "text": blk.text})
		case "tool_use":
			content = append(content, map[string]any{"type": "tool_use", "id": blk.id, "name": blk.name, "input": blk.input})
		case "thinking":
			content = append(content, map[string]any{"type": "thinking", "thinking": blk.text, "signature": blk.signature})
		}
	}
	return map[string]any{
		"id": a.id, "type": "message", "role": "assistant", "model": a.model,
		"content": content, "stop_reason": a.stopReason, "stop_sequence": a.stopSequence,
		"usage": a.usage,
	}
}

// statusFor maps an upstream error event's Anthropic type to the non-streaming
// response status; anything unrecognized is a 502 api_error.
func statusFor(upErrType string) (int, string) {
	switch upErrType {
	case "rate_limit_error":
		return http.StatusTooManyRequests, upErrType
	case "authentication_error":
		return http.StatusUnauthorized, upErrType
	default:
		return http.StatusBadGateway, "api_error"
	}
}
