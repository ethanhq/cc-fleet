package redact

import (
	"strings"
	"testing"
)

// TestMaskKeyLike_RemovesSentinel: every shape we worry about must have the
// sentinel disappear from the output, AND the canonical replacement must
// appear so the redaction is observable in dashboards / logs.
func TestMaskKeyLike_RemovesSentinel(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"sk-prefix", "vendor error: Invalid key sk-SENTINEL01234567890, please rotate"},
		{"sk-uppercase", "Invalid SK-SENTINEL01234567890"},
		{"bearer", "Authorization: Bearer sk-SENTINEL01234567890 was rejected"},
		{"bearer-lower", "header bearer sk-SENTINEL01234567890 leaked"},
		{"x-api-key", "request had x-api-key: sk-SENTINEL01234567890 set"},
		{"x-api-key-eq", "request had x-api-key=sk-SENTINEL01234567890 set"},
		{"embedded-mid", "before sk-SENTINEL01234567890 after"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out := MaskKeyLikeString(tc.in)
			if strings.Contains(out, "SENTINEL01234567890") {
				t.Fatalf("output still contains sentinel: %q", out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Fatalf("output missing canonical placeholder: %q", out)
			}
		})
	}
}

// TestMaskKeyLike_LeavesUnrelatedTextAlone: redactions must not eat normal
// content. A short token like "sk-true" (no plausible secret length) and a
// plain English word must survive.
func TestMaskKeyLike_LeavesUnrelatedTextAlone(t *testing.T) {
	keep := []string{
		"vendor returned HTTP 500",
		"sk-",                                 // bare prefix, no secret-shaped chars after
		"endpoint http://x.example/v1/models", // just URL
		"models_endpoint is missing",
	}
	for _, in := range keep {
		out := MaskKeyLikeString(in)
		if out != in {
			t.Errorf("MaskKeyLikeString(%q) changed to %q — should leave benign text alone", in, out)
		}
	}
}

// TestMaskKeyLike_EmptyAndNilSafe: zero values are common in error paths and
// must not allocate or panic.
func TestMaskKeyLike_EmptyAndNilSafe(t *testing.T) {
	if got := MaskKeyLike(nil); got != nil && len(got) != 0 {
		t.Errorf("MaskKeyLike(nil) = %q, want nil/empty", got)
	}
	if got := MaskKeyLike([]byte{}); len(got) != 0 {
		t.Errorf("MaskKeyLike([]) = %q, want empty", got)
	}
	if got := MaskKeyLikeString(""); got != "" {
		t.Errorf("MaskKeyLikeString(\"\") = %q, want empty", got)
	}
}

// TestMaskKeyLike_ByteZero: a sentinel at byte 0 of a body is the exact concern
// (truncation can't save you). Verify masking still kicks in.
func TestMaskKeyLike_ByteZero(t *testing.T) {
	const sentinel = "sk-SENTINEL01234567890"
	body := []byte(sentinel + "...vendor body...")
	out := MaskKeyLike(body)
	if strings.Contains(string(out), "SENTINEL01234567890") {
		t.Fatalf("byte-0 sentinel survived: %q", string(out))
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Fatalf("byte-0 sentinel not replaced with placeholder: %q", string(out))
	}
}
