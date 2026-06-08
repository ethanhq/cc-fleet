package codexproxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// fakeJWT builds an unsigned JWT whose payload is the given claims map.
func fakeJWT(claims map[string]any) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	pb, _ := json.Marshal(claims)
	return hdr + "." + base64.RawURLEncoding.EncodeToString(pb) + "."
}

func TestAccountIDFallbackChain(t *testing.T) {
	authClaim := map[string]any{jwtAuthClaim: map[string]any{"chatgpt_account_id": "acc-id"}}

	// 1) explicit field wins.
	if got := accountIDFromTokens(&tokens{AccountID: "explicit"}); got != "explicit" {
		t.Fatalf("explicit account id: %q", got)
	}
	// 2) id_token claim is used before the access token.
	tk := &tokens{IDToken: fakeJWT(authClaim)}
	if got := accountIDFromTokens(tk); got != "acc-id" {
		t.Fatalf("id_token account id: %q", got)
	}
	// 3) access_token claim is the last resort.
	tk = &tokens{AccessToken: fakeJWT(authClaim)}
	if got := accountIDFromTokens(tk); got != "acc-id" {
		t.Fatalf("access_token account id: %q", got)
	}
	// 4) none present.
	if got := accountIDFromTokens(&tokens{}); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestTokenExpiry(t *testing.T) {
	jwt := fakeJWT(map[string]any{"exp": float64(1781542266)})
	exp, ok := tokenExpiry(jwt)
	if !ok || exp.Unix() != 1781542266 {
		t.Fatalf("tokenExpiry=%v ok=%v", exp, ok)
	}
	if _, ok := tokenExpiry("not-a-jwt"); ok {
		t.Fatal("expected !ok for a non-JWT")
	}
}

func TestParseInterval(t *testing.T) {
	if d := parseInterval(json.RawMessage(`5`)); d.Seconds() != 8 { // 5 + 3 safety
		t.Fatalf("numeric interval=%v", d)
	}
	if d := parseInterval(nil); d.Seconds() != 8 {
		t.Fatalf("default interval=%v", d)
	}
}

func TestPortFromBaseURL(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"http://127.0.0.1:8765/", 8765, true},
		{"http://127.0.0.1:9000", 9000, true},
		{"http://127.0.0.1/", 0, false},
	} {
		got, err := PortFromBaseURL(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Fatalf("PortFromBaseURL(%q)=%d,%v", c.in, got, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("PortFromBaseURL(%q) expected error", c.in)
		}
	}
}

func TestIsCloudflareChallenge(t *testing.T) {
	h := http.Header{}
	h.Set("cf-mitigated", "challenge")
	if !isCloudflareChallenge(h, "") {
		t.Fatal("cf-mitigated header should be detected")
	}
	h2 := http.Header{}
	h2.Set("Server", "cloudflare")
	if !isCloudflareChallenge(h2, "Just a moment...") {
		t.Fatal("cloudflare challenge body should be detected")
	}
	if isCloudflareChallenge(http.Header{}, "ordinary 403 body") {
		t.Fatal("ordinary 403 must not be a cloudflare challenge")
	}
}

func TestQuotaMessage(t *testing.T) {
	got := quotaMessage(`{"error":{"code":"usage_limit_reached","resets_at":1781542266}}`)
	want := "codex usage_limit_reached (resets at 2026-06-15T16:51:06Z)"
	if got != want {
		t.Fatalf("quotaMessage = %q, want %q", got, want)
	}
	if got := quotaMessage("plain throttling text"); got != "codex usage limit reached: plain throttling text" {
		t.Fatalf("fallback = %q", got)
	}
}

func TestEnsureForVendor_RejectsNonLoopback(t *testing.T) {
	for _, c := range []struct {
		name, base, models string
		wantErr            bool
	}{
		{"remote base", "https://evil.example.com/", "http://127.0.0.1:17222/v1/models", true},
		{"http remote base", "http://evil.example.com:17222/", "http://127.0.0.1:17222/v1/models", true},
		{"userinfo base", "http://x@127.0.0.1:17222/", "http://127.0.0.1:17222/v1/models", true},
		{"remote models", "http://127.0.0.1:17222/", "https://evil.example.com/v1/models", true},
		{"no port", "http://127.0.0.1/", "http://127.0.0.1/v1/models", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := &config.Vendor{SecretBackend: SecretBackend, BaseURL: c.base, ModelsEndpoint: c.models}
			if err := EnsureForVendor(v); (err != nil) != c.wantErr {
				t.Fatalf("EnsureForVendor err=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
	// A non-codex vendor is always a no-op, even with a remote base_url.
	if err := EnsureForVendor(&config.Vendor{SecretBackend: "file", BaseURL: "https://api.example.com/"}); err != nil {
		t.Fatalf("non-codex vendor must be a no-op, got %v", err)
	}
}

func TestStaticModels(t *testing.T) {
	m := StaticModels()
	if len(m) == 0 || m[0] != "gpt-5.5" {
		t.Fatalf("StaticModels = %v", m)
	}
	m[0] = "mutated" // must not affect the package list
	if StaticModels()[0] != "gpt-5.5" {
		t.Fatal("StaticModels returned a shared backing array")
	}
}

func TestAnthropicErrorFor(t *testing.T) {
	for _, c := range []struct {
		kind   upstreamKind
		status int
		etype  string
	}{
		{upQuota, http.StatusTooManyRequests, "rate_limit_error"},
		{upCloudflare, http.StatusForbidden, "api_error"},
		{upAuth, http.StatusUnauthorized, "authentication_error"},
		{upBadRequest, http.StatusBadRequest, "invalid_request_error"},
	} {
		status, etype, _ := anthropicErrorFor(&upstreamError{kind: c.kind, status: c.status, message: "m"})
		if status != c.status || etype != c.etype {
			t.Fatalf("anthropicErrorFor(%d)=%d,%s want %d,%s", c.kind, status, etype, c.status, c.etype)
		}
	}
}
