package codexproxy

import "testing"

func TestSessionKey(t *testing.T) {
	cases := []struct{ name, userID, want string }{
		{"empty", "", ""},
		{"session id", `{"device_id":"dev-1","account_uuid":"acct-1","session_id":"sess-1"}`, "sess-1"},
		{"teammate keeps its own session", `{"device_id":"dev-1","account_uuid":"","session_id":"sess-1","parent_session_id":"lead-1"}`, "sess-1"},
		// The key order Claude Code emits when CLAUDE_CODE_EXTRA_METADATA names both keys.
		{"exact key only", `{"session_id":"sess-1","SESSION_ID":"extra","device_id":"dev-1"}`, "sess-1"},
		{"object without a session id", `{"device_id":"dev-1","account_uuid":"acct-1"}`, ""},
		{"empty session id", `{"device_id":"dev-1","session_id":""}`, ""},
		{"non-string session id", `{"device_id":"dev-1","session_id":7}`, ""},
		{"json null", `null`, ""},
		// Pinned so the key stays the same across daemon restarts and releases.
		{"legacy string", "user_abc_session_123", "4c900ce79a89cf62cade79ffa195ec8e"},
		{"json string", `"sess-1"`, "fd5409dcc49c26efac8569a3467e00de"},
	}
	for _, c := range cases {
		if got := sessionKey(c.userID); got != c.want {
			t.Errorf("%s: sessionKey(%s) = %q, want %q", c.name, c.userID, got, c.want)
		}
	}
}
