// Package redact masks key-like tokens in any text that may flow into a JSON
// envelope or error message. It is the single canonical sanitizer.
//
// The goal is NOT bulletproof secret scrubbing — it is defense-in-depth on
// surfaces that should never contain a key. Callers should still avoid building
// error strings out of raw response bodies; this is the last line of defense.
package redact

import (
	"regexp"
)

// keyPatterns matches the common shapes a leaked key takes in the wild:
//   - "sk-..." plus 8+ alphanumeric/underscore/hyphen bytes (covers Anthropic,
//     OpenAI, DeepSeek, GLM, and most OpenAI-compat shims — including the
//     uppercase-prefix variants some vendors use).
//   - "Bearer <token>" / "bearer <token>" — when a header is echoed back, the
//     prefix itself is usually present.
//   - "x-api-key: <token>" / "x-api-key=<token>" — verbose vendor error logs
//     occasionally include the response header dump.
//
// Each pattern is wrapped with non-capturing groups so the replacement uses the
// matched prefix verbatim and substitutes only the secret-looking part.
var keyPatterns = []*regexp.Regexp{
	// sk-XXXXX… (anchored on the "sk-" prefix; 8+ chars after to avoid masking
	// "sk-true" / "sk-false" word-like accidents while still catching keys).
	regexp.MustCompile(`(?i)sk-[A-Za-z0-9_\-]{8,}`),
	// Bearer <token>
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]{8,}`),
	// x-api-key: <token> (header echo)
	regexp.MustCompile(`(?i)x-api-key\s*[:=]\s*[A-Za-z0-9._\-]{8,}`),
}

// keyRedactedPlaceholder is the canonical replacement we splice in. Stable so
// tests can grep for it and skill consumers can recognize a redaction occurred.
const keyRedactedPlaceholder = "sk-[REDACTED]"

// MaskKeyLike replaces every key-like substring in b with a canonical
// placeholder. b is consumed read-only; a fresh slice is returned. nil/empty
// input returns the input unchanged.
func MaskKeyLike(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	out := b
	for _, pat := range keyPatterns {
		out = pat.ReplaceAll(out, []byte(keyRedactedPlaceholder))
	}
	return out
}

// MaskKeyLikeString is the string analogue of MaskKeyLike — convenience for
// callers that already hold a string (e.g. error messages).
func MaskKeyLikeString(s string) string {
	if s == "" {
		return s
	}
	return string(MaskKeyLike([]byte(s)))
}
