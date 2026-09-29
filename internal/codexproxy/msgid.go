package codexproxy

import (
	"crypto/rand"
	"encoding/hex"
)

// newMessageID returns a fresh Anthropic-style message id. Each converted
// response needs its own: Claude Code merges consecutive turns that share an id,
// scrambling the conversation history.
func newMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return "msg_" + hex.EncodeToString(b)
}
