package subagent

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Shared cross-platform test fixtures and assertion helpers. The unix-only
// process/exec helpers (writeFakeBin, writeMinimalVendors, deadReapedPID,
// readPID, waitGone) live in helpers_unix_test.go.

// Real inner envelopes captured from a smoke run.
const smokeSuccessJSON = `{"type":"result","subtype":"success","is_error":false,"api_error_status":null,
 "duration_ms":3654,"duration_api_ms":3385,"ttft_ms":3397,"num_turns":1,"stop_reason":"end_turn",
 "session_id":"84c5b474-aaaa","total_cost_usd":0.258409,
 "usage":{"input_tokens":50750,"cache_read_input_tokens":18,"output_tokens":186,"service_tier":"standard"},
 "modelUsage":{"mimo-v2-flash":{"inputTokens":50750,"outputTokens":186,"costUSD":0.258409}},
 "result":"SUBAGENT_SMOKE_OK=42","permission_denials":[],"terminal_reason":"completed"}`

const smoke429BalanceJSON = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,
 "duration_ms":178257,"duration_api_ms":0,"num_turns":1,"stop_reason":"stop_sequence",
 "result":"API Error: Request rejected (429) · [1113][余额不足或无可用资源包,请充值。]",
 "total_cost_usd":0,"modelUsage":{},"permission_denials":[],"terminal_reason":"completed"}`

// regSyncJob mints a sync job id and registers it (full profile), returning the
// id — the pre-split registerSyncJob shape the board tests rely on.
func regSyncJob(req Request, model string) string {
	jobID := mintSyncJobID()
	registerSyncJob(jobID, req, model, "", "")
	return jobID
}

// assertJSONEq compares two JSON documents structurally (key order and
// whitespace are irrelevant).
func assertJSONEq(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not valid JSON: %v (%q)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not valid JSON: %v (%q)", err, want)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("JSON mismatch:\n got %s\nwant %s", got, want)
	}
}

// ----- argv assertion helpers -----

func idxOf(argv []string, tok string) int {
	for i, a := range argv {
		if a == tok {
			return i
		}
	}
	return -1
}

// assertSeq checks that the given tokens appear as a contiguous run in argv.
func assertSeq(t *testing.T, argv []string, seq ...string) {
	t.Helper()
	joined := strings.Join(argv, "\x00")
	want := strings.Join(seq, "\x00")
	if !strings.Contains(joined, want) {
		t.Fatalf("argv %v does not contain contiguous %v", argv, seq)
	}
}

// assertPairAfter checks flag is present and immediately followed by val.
func assertPairAfter(t *testing.T, argv []string, flag, val string) {
	t.Helper()
	i := idxOf(argv, flag)
	if i < 0 || i+1 >= len(argv) || argv[i+1] != val {
		t.Fatalf("expected %s %s in argv: %v", flag, val, argv)
	}
}

func assertAbsent(t *testing.T, argv []string, tok string) {
	t.Helper()
	if idxOf(argv, tok) >= 0 {
		t.Fatalf("expected %q absent from argv: %v", tok, argv)
	}
}

// argvHasAdjacent reports whether flag is immediately followed by value in argv.
func argvHasAdjacent(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// failingReader returns partial bytes then an error, simulating a network /
// piped stdin failure mid-stream. launchBackground must catch the error BEFORE
// calling cmd.Start so claude never receives a partial prompt.
type failingReader struct {
	partial []byte
	err     error
	read    bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	n := copy(p, r.partial)
	return n, r.err
}

// Compile-time guard: failingReader satisfies io.Reader.
var _ io.Reader = (*failingReader)(nil)
