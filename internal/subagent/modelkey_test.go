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

// A key matching the resolved request confirms the requested route was billed
// and wins outright — modelUsage bills Agent-tool subagents into the same map,
// so a delegate can out-token the main model without having been the route.
func TestModelKeyPrefersRequestedRoute(t *testing.T) {
	cases := []struct {
		name     string
		usage    map[string]json.RawMessage
		fallback string
		want     string
	}{
		{"exact id present but out-tokened by a delegate", map[string]json.RawMessage{
			"gpt-5.6-terra":             usage(3, 40, 900, 0),
			"claude-haiku-4-5-20251001": usage(50000, 9000, 200000, 0),
		}, "gpt-5.6-terra", "gpt-5.6-terra"},
		{"native alias matches its id family", map[string]json.RawMessage{
			"claude-sonnet-5":           usage(2, 4, 0, 6),
			"claude-haiku-4-5-20251001": usage(80000, 12000, 0, 0),
		}, "sonnet", "claude-sonnet-5"},
		{"trailing 1m marker on the resolved id still matches", map[string]json.RawMessage{
			"glm-4.6":     usage(5, 9, 0, 0),
			"other-model": usage(7000, 800, 0, 0),
		}, "glm-4.6[1m]", "glm-4.6"},
		{"several matching keys: dominant among them", map[string]json.RawMessage{
			"claude-sonnet-5":           usage(10, 10, 0, 0),
			"claude-sonnet-4-5":         usage(900, 90, 0, 0),
			"claude-haiku-4-5-20251001": usage(99999, 9999, 0, 0),
		}, "sonnet", "claude-sonnet-4-5"},
		{"zero-usage requested key still beats a busy delegate", map[string]json.RawMessage{
			"gpt-5.6-terra": usage(0, 0, 0, 0),
			"gpt-5.6-sol":   usage(500, 60, 0, 0),
		}, "gpt-5.6-terra", "gpt-5.6-terra"},
		{"request absent from the bill: dominance over all keys", map[string]json.RawMessage{
			"substitute-model": usage(400, 80, 0, 0),
			"helper-model":     usage(20, 3, 0, 0),
		}, "asked-for-model", "substitute-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				if got := modelKey(tc.usage, tc.fallback); got != tc.want {
					t.Fatalf("round %d: modelKey = %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}

// Fuzzy matching is bounded: a literal id matches exactly (a request for gpt-5
// billed only as gpt-5.5 is a substitution, not a route confirmation), and only
// the native alias words match by family.
func TestModelKeyMatchBounds(t *testing.T) {
	cases := []struct {
		name     string
		usage    map[string]json.RawMessage
		fallback string
		want     string
	}{
		{"literal prefix does not steal the route", map[string]json.RawMessage{
			"gpt-5.5":     usage(900, 90, 0, 0),
			"other-model": usage(5000, 700, 0, 0),
		}, "gpt-5", "other-model"},
		{"exact literal beats a busier family sibling", map[string]json.RawMessage{
			"claude-sonnet-5":   usage(5, 8, 0, 0),
			"claude-sonnet-4-5": usage(9000, 900, 0, 0),
		}, "claude-sonnet-5", "claude-sonnet-5"},
		{"exact match is case-insensitive", map[string]json.RawMessage{
			"GLM-4.6":     usage(3, 6, 0, 0),
			"other-model": usage(800, 90, 0, 0),
		}, "glm-4.6", "GLM-4.6"},
		{"alias still matches its family", map[string]json.RawMessage{
			"claude-sonnet-5":           usage(2, 4, 0, 6),
			"claude-haiku-4-5-20251001": usage(80000, 12000, 0, 0),
		}, "sonnet", "claude-sonnet-5"},
		{"fable alias matches its family against a busy delegate", map[string]json.RawMessage{
			"claude-fable-5":            usage(4, 30, 0, 12),
			"claude-haiku-4-5-20251001": usage(70000, 9000, 0, 0),
		}, "fable", "claude-fable-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 50; i++ {
				if got := modelKey(tc.usage, tc.fallback); got != tc.want {
					t.Fatalf("round %d: modelKey = %q, want %q", i, got, tc.want)
				}
			}
		})
	}
}
