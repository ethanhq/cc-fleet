package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validOpenAIChat returns an openai-chat Vendor that should pass Validate: a
// loopback daemon base_url + a real upstream_url + a real-key backend.
func validOpenAIChat(name string) *Vendor {
	v := validVendor(name)
	v.Protocol = ProtocolOpenAIChat
	v.BaseURL = "http://127.0.0.1:17240/"
	v.UpstreamURL = "https://api.openai.com/v1"
	v.ModelsEndpoint = "https://api.openai.com/v1/models"
	v.SecretBackend = "file"
	return v
}

// A codex row predating the protocol field (codex-oauth backend, no protocol)
// resolves to the codex protocol and validates exactly as it did before.
func TestProtocolCompatNormalize(t *testing.T) {
	v := validVendor("codex")
	v.SecretBackend = CodexOAuthBackend
	v.SecretRef = CodexOAuthBackend
	v.BaseURL = "http://127.0.0.1:17222/"
	v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
	v.Protocol = "" // shipped codex rows carry no protocol line

	if got := v.EffectiveProtocol(); got != ProtocolCodexOAuth {
		t.Fatalf("EffectiveProtocol = %q, want codex-oauth", got)
	}
	if !v.DaemonBacked() {
		t.Fatal("a codex row must be daemon-backed")
	}
	if err := v.validate("codex"); err != nil {
		t.Fatalf("protocol-less codex row must validate: %v", err)
	}
}

func TestProtocolClosedSet(t *testing.T) {
	v := validVendor("x")
	v.Protocol = "openai-completions" // not in the closed set
	if err := v.validate("x"); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("unknown protocol must be rejected, got %v", err)
	}
}

func TestValidateWire(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Vendor)
		wantErr string // substring; "" = must pass
	}{
		{"openai-chat ok", func(v *Vendor) {}, ""},
		{"openai-responses ok", func(v *Vendor) { v.Protocol = ProtocolOpenAIResponses }, ""},
		{"openai missing upstream_url", func(v *Vendor) { v.UpstreamURL = "" }, "requires upstream_url"},
		{"openai with codex backend", func(v *Vendor) { v.SecretBackend = CodexOAuthBackend; v.SecretRef = CodexOAuthBackend }, "not the codex-oauth backend"},
		{"openai non-loopback base", func(v *Vendor) { v.BaseURL = "https://api.openai.com/" }, "base_url"},
		{"openai bad upstream", func(v *Vendor) { v.UpstreamURL = "http://api.openai.com/v1" }, "upstream_url"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := validOpenAIChat("oai")
			c.mutate(v)
			err := v.validate("oai")
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}

	// codex-oauth protocol cross-checks.
	t.Run("codex wrong backend", func(t *testing.T) {
		v := validVendor("c")
		v.Protocol = ProtocolCodexOAuth
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		if err := v.validate("c"); err == nil || !strings.Contains(err.Error(), "requires secret_backend") {
			t.Fatalf("codex protocol needs the codex backend, got %v", err)
		}
	})
	t.Run("codex with upstream_url", func(t *testing.T) {
		v := validVendor("c")
		v.SecretBackend = CodexOAuthBackend
		v.SecretRef = CodexOAuthBackend
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		v.UpstreamURL = "https://api.openai.com/v1"
		if err := v.validate("c"); err == nil || !strings.Contains(err.Error(), "must not set upstream_url") {
			t.Fatalf("codex must reject upstream_url, got %v", err)
		}
	})

	// Anthropic-native must not carry upstream_url.
	t.Run("anthropic with upstream_url", func(t *testing.T) {
		v := validVendor("a")
		v.UpstreamURL = "https://api.openai.com/v1"
		if err := v.validate("a"); err == nil || !strings.Contains(err.Error(), "only valid for an openai-*") {
			t.Fatalf("anthropic-native must reject upstream_url, got %v", err)
		}
	})
}

func TestValidateUpstreamURL(t *testing.T) {
	ok := []string{
		"https://api.openai.com/v1",
		"https://api.groq.com/openai/v1",
		"https://api.fireworks.ai/inference/v1",
		"https://openrouter.ai/api/v1",
		"http://127.0.0.1:11434/v1",
		"http://localhost:8000/v1",
		"https://api.openai.com/v1/", // a single trailing slash is tolerated
	}
	for _, u := range ok {
		if err := ValidateUpstreamURL(u); err != nil {
			t.Errorf("ValidateUpstreamURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"http://api.openai.com/v1",         // http for a remote host
		"https://x@api.openai.com/v1",      // userinfo
		"https://api.openai.com/v1?k=1",    // query
		"https://api.openai.com/v1#frag",   // fragment
		"https://api.openai.com/v1/../etc", // unsafe path
		"ftp://api.openai.com/v1",          // scheme
		"https:///v1",                      // missing host
		" https://api.openai.com/v1",       // leading space (daemon uses it verbatim)
		"https://api.openai.com/v1 ",       // trailing space
	}
	for _, u := range bad {
		if err := ValidateUpstreamURL(u); err == nil {
			t.Errorf("ValidateUpstreamURL(%q) = nil, want error", u)
		}
	}
}

// An existing Anthropic-native config (no protocol/upstream_url lines) is
// byte-stable through Save: omitempty must not introduce either field.
func TestAnthropicNativeSaveByteStable(t *testing.T) {
	cfg := &Config{Version: SchemaVersion, Vendors: map[string]*Vendor{
		"deepseek": validVendor("deepseek"),
	}}
	p := filepath.Join(t.TempDir(), "vendors.toml")
	if err := SaveToPath(cfg, p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "protocol") || strings.Contains(string(b), "upstream_url") {
		t.Fatalf("Anthropic-native row gained a protocol/upstream_url line:\n%s", b)
	}
}
