package subagent

import (
	"strings"
	"testing"
)

// TestFailWithPreview_MasksKeyLikeInJSON: when claude exits without a parseable
// result envelope and writes a key-like substring to stderr, the JSON ErrorMsg
// must NOT contain that substring. The text-mode print path intentionally still
// shows raw stderr to a human operator — masking applies to the JSON contract
// only, which is what skills consume.
func TestFailWithPreview_MasksKeyLikeInJSON(t *testing.T) {
	const sentinel = "sk-SENTINEL01234567890"
	stderr := []byte("provider logged: " + sentinel + " was rejected")

	res := failWithPreview(Request{Provider: "glm", JSON: true}, stderr, 1)
	if res.OK {
		t.Fatal("failWithPreview should produce a failure Result")
	}
	if res.ErrorCode != ErrCodeFailed {
		t.Fatalf("ErrorCode = %q, want SUBAGENT_FAILED", res.ErrorCode)
	}
	if strings.Contains(res.ErrorMsg, "SENTINEL01234567890") {
		t.Fatalf("JSON ErrorMsg leaked sentinel: %q", res.ErrorMsg)
	}
	if !strings.Contains(res.ErrorMsg, "[REDACTED]") {
		t.Fatalf("JSON ErrorMsg missing redaction marker: %q", res.ErrorMsg)
	}
}

// TestClassify_MasksKeyLikeOnNoEnvelopePath: the higher-level classify entry
// point (the one Run() actually calls) must also mask. We feed it stdout
// that's NOT a valid envelope so it falls through to failWithPreview.
func TestClassify_MasksKeyLikeOnNoEnvelopePath(t *testing.T) {
	const sentinel = "sk-SENTINEL01234567890"
	// innerJSON=true but stdout is empty / garbage → parseInner returns false
	// → failWithPreview path. Stderr carries the sentinel.
	stderr := []byte("Authorization: Bearer " + sentinel + " failed")
	res := classify(Request{Provider: "glm", JSON: true}, "glm-4.6", nil, stderr, 1, false, true)
	if res.OK {
		t.Fatalf("classify should report failure on no-envelope path")
	}
	if strings.Contains(res.ErrorMsg, "SENTINEL01234567890") {
		t.Fatalf("classify ErrorMsg leaked sentinel: %q", res.ErrorMsg)
	}
}

// TestClassify_TextModeDeadJobIsFailed: an exit-0 process that wrote nothing to
// stdout but did write to stderr must NOT be reported as success.
func TestClassify_TextModeDeadJobIsFailed(t *testing.T) {
	stderr := []byte("something went wrong before any output")
	res := classify(Request{Provider: "glm"}, "glm-4.6", nil, stderr, 0, false, false)
	if res.OK {
		t.Fatalf("text-mode dead job (empty stdout + stderr) must classify as failure, got OK=true")
	}
	if res.ErrorCode != ErrCodeFailed {
		t.Fatalf("ErrorCode = %q, want SUBAGENT_FAILED", res.ErrorCode)
	}
}

// TestClassify_TextModeSuccessStillSucceeds: control — the normal text-mode
// success path (exit 0, non-empty stdout) must keep working.
func TestClassify_TextModeSuccessStillSucceeds(t *testing.T) {
	res := classify(Request{Provider: "glm"}, "glm-4.6", []byte("hello"), nil, 0, false, false)
	if !res.OK {
		t.Fatalf("text-mode success path regressed: OK=false code=%s", res.ErrorCode)
	}
	if res.Result != "hello" {
		t.Errorf("Result = %q, want %q", res.Result, "hello")
	}
}
