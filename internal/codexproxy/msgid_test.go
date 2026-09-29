package codexproxy

import (
	"strings"
	"testing"
)

// Every converted response carries its own message id, generated once per
// converter: Claude Code merges turns that share an id.
func TestConvertersEmitUniqueMessageIDs(t *testing.T) {
	converters := map[string]func(sink sseSink) error{
		"responses": func(sink sseSink) error {
			return newStreamConverter(sink, ccTest("gpt-5.5")).Convert(strings.NewReader(
				"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"delta\":\"hi\"}\n\n"))
		},
		"chat": func(sink sseSink) error {
			return newChatStreamConverter(sink, ccTest("gpt-x")).Convert(strings.NewReader(
				"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		},
	}
	seen := map[string]bool{}
	for name, run := range converters {
		for i := 0; i < 3; i++ {
			sink := &recSink{}
			if err := run(sink); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if n := strings.Count(sink.seq(), "message_start"); n != 1 {
				t.Fatalf("%s: %d message_start events", name, n)
			}
			msg, _ := sink.payload("message_start", 0)["message"].(map[string]any)
			id, _ := msg["id"].(string)
			if !cpMsgID.MatchString(id) {
				t.Fatalf("%s: id %q is not msg_ + 24 hex", name, id)
			}
			if seen[id] {
				t.Fatalf("%s: id %q reused", name, id)
			}
			seen[id] = true
		}
	}
}
