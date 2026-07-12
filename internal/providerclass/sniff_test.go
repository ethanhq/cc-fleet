package providerclass

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shapeServer answers every request with status + body, echoing the shapes real
// providers answer an unauthenticated POST /v1/messages with.
func shapeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// TestMessagesRouteMissing: ONLY a bare 404 is protocol-mismatch evidence.
// Every real-provider auth-failure shape — envelope, plain text, flat JSON,
// OpenAI-shaped — and every transport failure must pass (false).
func TestMessagesRouteMissing(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"route 404 (openai endpoint)", http.StatusNotFound, "404 page not found", true},
		{"401 anthropic envelope", http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"x-api-key header is required"}}`, false},
		{"401 plain text (deepseek shape)", http.StatusUnauthorized, "Authentication Fails (governor)", false},
		{"403 flat json (qwen shape)", http.StatusForbidden, `{"message":"api-key is required","type":"authentication_error"}`, false},
		{"401 openai-shaped (glm/kimi shape)", http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided","type":"incorrect_api_key_error"}}`, false},
		{"400 bad request", http.StatusBadRequest, `{"error":"bad"}`, false},
		{"500 server error", http.StatusInternalServerError, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := shapeServer(t, tc.status, tc.body)
			if got := MessagesRouteMissing(s.URL); got != tc.want {
				t.Errorf("MessagesRouteMissing(%d %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
			// A trailing slash on base_url must not double the path separator.
			if got := MessagesRouteMissing(s.URL + "/"); got != tc.want {
				t.Errorf("MessagesRouteMissing with trailing slash = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("404 with anthropic envelope passes", func(t *testing.T) {
		// An anonymously-reachable Anthropic-protocol gateway resolves the probe's
		// dummy model to a genuine Anthropic 404 — that is NOT mismatch evidence.
		s := shapeServer(t, http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"model: x"}}`)
		if MessagesRouteMissing(s.URL) {
			t.Error("an Anthropic-enveloped 404 must not read as protocol mismatch")
		}
	})

	t.Run("transport failure fails open", func(t *testing.T) {
		s := shapeServer(t, http.StatusNotFound, "")
		s.Close() // conn-refused from here on
		if MessagesRouteMissing(s.URL) {
			t.Error("a connection failure must not read as protocol mismatch")
		}
	})

	t.Run("redirect is not followed into a 404", func(t *testing.T) {
		// A canonicalizing endpoint 301s the POST; following it would GET the
		// target (Go rewrites the method) where a 404 would false-trip. The
		// sniff must judge the 3xx itself → pass.
		notFound := shapeServer(t, http.StatusNotFound, "404 page not found")
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, notFound.URL+r.URL.Path, http.StatusMovedPermanently)
		}))
		t.Cleanup(s.Close)
		if MessagesRouteMissing(s.URL) {
			t.Error("a redirecting endpoint must not read as protocol mismatch")
		}
	})

	t.Run("credential-bearing base_url is never probed", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("a base_url with userinfo/query/fragment must not be probed")
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(s.Close)
		host := strings.TrimPrefix(s.URL, "http://")
		for _, base := range []string{
			"http://user:secret@" + host,
			s.URL + "?key=secret",
			s.URL + "#frag",
		} {
			if MessagesRouteMissing(base) {
				t.Errorf("MessagesRouteMissing(%q) = true, want fail-open", base)
			}
		}
	})

	t.Run("escaped path segments survive the join", func(t *testing.T) {
		// Joining must go through the URL (u.JoinPath), not u.Path — a naive
		// Path append leaves RawPath stale, flattening %2F and probing a path
		// the endpoint never served (a false trip on a working provider).
		var escaped string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			escaped = r.URL.EscapedPath()
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(s.Close)
		MessagesRouteMissing(s.URL + "/a%2Fb")
		if escaped != "/a%2Fb/v1/messages" {
			t.Errorf("probed escaped path %q, want /a%%2Fb/v1/messages", escaped)
		}
	})

	t.Run("probe shape: POST /v1/messages, versioned, no credentials", func(t *testing.T) {
		var method, path, version, auth, apiKey string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			method, path = r.Method, r.URL.Path
			version = r.Header.Get("anthropic-version")
			auth = r.Header.Get("Authorization")
			apiKey = r.Header.Get("x-api-key")
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(s.Close)
		MessagesRouteMissing(s.URL + "/anthropic")
		if method != http.MethodPost || path != "/anthropic/v1/messages" {
			t.Errorf("probed %s %s, want POST /anthropic/v1/messages", method, path)
		}
		if version == "" {
			t.Error("probe must carry anthropic-version")
		}
		if auth != "" || apiKey != "" {
			t.Errorf("probe must carry no credentials, got Authorization=%q x-api-key=%q", auth, apiKey)
		}
	})
}
