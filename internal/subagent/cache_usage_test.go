package subagent

import "testing"

// A success envelope's cache_creation_input_tokens (the prompt-cache WRITE cost) is parsed onto
// Result.Usage alongside input/output/cache-read, so the board can surface it.
func TestClassifyCarriesCacheCreationTokens(t *testing.T) {
	js := `{"type":"result","is_error":false,"result":"ok","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":7}}`
	res := classify(Request{Provider: "v", JSON: true}, "m", []byte(js), nil, 0, false, true)
	if res.Usage == nil {
		t.Fatal("nil usage")
	}
	if res.Usage.CacheCreationInputTokens != 7 {
		t.Errorf("cache_creation = %d, want 7", res.Usage.CacheCreationInputTokens)
	}
}
