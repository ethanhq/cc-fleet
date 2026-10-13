package codexproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// sessionKey derives the per-conversation value an upstream may see from
// metadata.user_id. Claude Code sends a JSON object there that also carries the
// install's device id and the Claude account uuid, so only its session_id is
// used, and an object without one (or a JSON null) yields no key. Any other
// value (older clients sent one plain string) becomes a digest.
// A teammate's parent_session_id is ignored: each conversation keeps its own key.
func sessionKey(userID string) string {
	if userID == "" {
		return ""
	}
	// A map, not a tagged struct: a struct field also matches SESSION_ID.
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(userID), &obj) == nil {
		var id string
		_ = json.Unmarshal(obj["session_id"], &id)
		return id
	}
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:16])
}
