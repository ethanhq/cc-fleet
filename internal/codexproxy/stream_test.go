package codexproxy

import (
	"io"
	"strings"
	"testing"
)

// recSink records emitted Anthropic events (names + payloads) for assertions.
type recSink struct {
	events   []string
	payloads []any
}

func (r *recSink) event(name string, data any) error {
	r.events = append(r.events, name)
	r.payloads = append(r.payloads, data)
	return nil
}

func (r *recSink) seq() string { return strings.Join(r.events, ",") }

// payload returns the i-th payload (0 = first) of the named event, or nil.
func (r *recSink) payload(name string, i int) map[string]any {
	for j, n := range r.events {
		if n != name {
			continue
		}
		if i == 0 {
			m, _ := r.payloads[j].(map[string]any)
			return m
		}
		i--
	}
	return nil
}

func runConvert(t *testing.T, sse string) *recSink {
	t.Helper()
	sink := &recSink{}
	c := newStreamConverter(sink, ccTest("gpt-5.5"))
	if err := c.Convert(strings.NewReader(sse)); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return sink
}

func TestConvert_TextStreamGrammar(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"m1","delta":"pong"}`,
		`data: {"type":"response.output_item.done","item_id":"m1"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	got := runConvert(t, sse).seq()
	want := "message_start,content_block_start,content_block_delta,content_block_stop,message_delta,message_stop"
	if got != want {
		t.Fatalf("event sequence:\n got=%s\nwant=%s", got, want)
	}
}

func TestConvert_ToolCallStreamsArgsAndSetsStopReason(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"f1","type":"function_call","call_id":"toolu_1","name":"grep"}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"f1","delta":"{\"q\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"f1","delta":"\"x\"}"}`,
		`data: {"type":"response.output_item.done","item_id":"f1"}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n\n") + "\n\n"
	sink := runConvert(t, sse)
	if got := sink.seq(); got != "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop" {
		t.Fatalf("tool-call sequence: %s", got)
	}
}

func TestConvert_AlwaysClosesOnTruncatedStream(t *testing.T) {
	// An open block with no done/completed must still close + emit message_stop.
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"m1","delta":"partial"}`,
	}, "\n\n") + "\n\n"
	got := runConvert(t, sse).seq()
	if !strings.HasSuffix(got, "content_block_stop,message_delta,message_stop") {
		t.Fatalf("truncated stream did not close cleanly: %s", got)
	}
}

func TestConvert_EmptyStreamStillEndsTurn(t *testing.T) {
	got := runConvert(t, "").seq()
	if got != "message_start,message_delta,message_stop" {
		t.Fatalf("empty stream: %s", got)
	}
}

func TestConvert_ThinkingSignatureFromEncryptedContent(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs1","type":"reasoning"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs1","delta":"thinking..."}`,
		`data: {"type":"response.output_item.done","item_id":"rs1","item":{"id":"rs1","type":"reasoning","encrypted_content":"gAAAAA-enc"}}`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
	}, "\n\n") + "\n\n"
	sink := runConvert(t, sse)
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop,message_delta,message_stop"
	if got := sink.seq(); got != want {
		t.Fatalf("thinking sequence:\n got=%s\nwant=%s", got, want)
	}
	// The second content_block_delta is the signature carrying encrypted_content.
	sig := sink.payload("content_block_delta", 1)
	delta, _ := sig["delta"].(map[string]any)
	if delta["type"] != "signature_delta" || delta["signature"] != "gAAAAA-enc" {
		t.Fatalf("signature delta = %v", delta)
	}
}

func TestConvert_FailedStreamEmitsErrorNotCleanStop(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"m1","delta":"par"}`,
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"upstream exploded"}}}`,
	}, "\n\n") + "\n\n"
	sink := runConvert(t, sse)
	want := "message_start,content_block_start,content_block_delta,content_block_stop,error"
	if got := sink.seq(); got != want {
		t.Fatalf("failed-stream sequence:\n got=%s\nwant=%s", got, want)
	}
	p := sink.payload("error", 0)
	inner, _ := p["error"].(map[string]any)
	msg, _ := inner["message"].(string)
	if !strings.Contains(msg, "upstream exploded") {
		t.Fatalf("error message = %q", msg)
	}
}

// errAfterReader yields data once, then a non-EOF error — a mid-stream transport
// failure. The converter must surface an Anthropic error event, not a clean stop.
type errAfterReader struct {
	data string
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(p, r.data), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestConvert_ReadErrorEmitsErrorNotCleanStop(t *testing.T) {
	data := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"m1","type":"message"}}`,
		`data: {"type":"response.output_text.delta","item_id":"m1","delta":"par"}`,
		"", // ensure the last data line is newline-terminated for the scanner
	}, "\n\n")
	sink := &recSink{}
	c := newStreamConverter(sink, ccTest("gpt-5.5"))
	err := c.Convert(&errAfterReader{data: data})
	if err == nil {
		t.Fatal("Convert must return the read error, not swallow it")
	}
	if last := sink.events[len(sink.events)-1]; last != "error" {
		t.Fatalf("a read error must end on an error event, got seq: %s", sink.seq())
	}
	for _, e := range sink.events {
		if e == "message_stop" {
			t.Fatalf("a read error must NOT emit a clean message_stop: %s", sink.seq())
		}
	}
}

func TestConvert_UsageSplitsCachedTokens(t *testing.T) {
	sse := `data: {"type":"response.completed","response":{"status":"completed",` +
		`"usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60}}}}` + "\n\n"
	sink := runConvert(t, sse)
	usage, _ := sink.payload("message_delta", 0)["usage"].(map[string]any)
	if usage["input_tokens"] != 40 || usage["cache_read_input_tokens"] != 60 || usage["output_tokens"] != 7 {
		t.Fatalf("usage = %v", usage)
	}
}
