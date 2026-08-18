package subagent

import (
	"encoding/json"
	"testing"
)

func usage(in, out, cacheRead, cacheCreate int64) json.RawMessage {
	b, _ := json.Marshal(map[string]int64{
		"inputTokens":              in,
		"outputTokens":             out,
		"cacheReadInputTokens":     cacheRead,
		"cacheCreationInputTokens": cacheCreate,
	})
	return b
}

// The billed-model key is the dominant one by total tokens, never map order.
func TestModelKeyPicksDominantModel(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]json.RawMessage
		want  string
	}{
		{"single key", map[string]json.RawMessage{
			"claude-sonnet-5": usage(2, 4, 3289, 0),
		}, "claude-sonnet-5"},
		// The real-world trap: a cache-warm main model has tiny raw input/output,
		// so a helper call out-tokens it on either alone — only the cache-read
		// column keeps the main model dominant.
		{"main model dominant via cache reads", map[string]json.RawMessage{
			"claude-sonnet-5":           usage(2, 4, 3289, 0),
			"claude-haiku-4-5-20251001": usage(900, 12, 0, 0),
		}, "claude-sonnet-5"},
		// A captured cold-cache envelope: the main model's whole context lands in
		// cacheCreation, so input+output alone would crown the helper 916 to 6.
		{"cold cache, dominant via cache creation", map[string]json.RawMessage{
			"claude-sonnet-5":           usage(2, 4, 0, 28903),
			"claude-haiku-4-5-20251001": usage(902, 14, 0, 0),
		}, "claude-sonnet-5"},
		{"unparseable usage counts as zero", map[string]json.RawMessage{
			"claude-sonnet-5":           json.RawMessage(`"garbage"`),
			"claude-haiku-4-5-20251001": usage(10, 5, 0, 0),
		}, "claude-haiku-4-5-20251001"},
		{"all-zero tie breaks lexicographically", map[string]json.RawMessage{
			"model-b": usage(0, 0, 0, 0),
			"model-a": usage(0, 0, 0, 0),
		}, "model-a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 50 rounds so a map-order dependence cannot hide behind one lucky
			// iteration.
			for i := 0; i < 50; i++ {
				if got := modelKey(tc.usage, "req-fallback"); got != tc.want {
					t.Fatalf("round %d: modelKey = %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

// An empty or keyless map falls back to the request's resolved model.
func TestModelKeyFallsBack(t *testing.T) {
	if got := modelKey(nil, "req-fallback"); got != "req-fallback" {
		t.Fatalf("nil map: %q, want fallback", got)
	}
	if got := modelKey(map[string]json.RawMessage{}, "req-fallback"); got != "req-fallback" {
		t.Fatalf("empty map: %q, want fallback", got)
	}
	if got := modelKey(map[string]json.RawMessage{"": usage(9, 9, 9, 9)}, "req-fallback"); got != "req-fallback" {
		t.Fatalf("empty-string key: %q, want fallback", got)
	}
}
