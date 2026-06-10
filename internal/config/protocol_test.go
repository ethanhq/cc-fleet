package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validOpenAIChat returns an openai-chat Provider that should pass Validate: a
// loopback daemon base_url + a real upstream_url + a real-key backend.
func validOpenAIChat(name string) *Provider {
	v := validProvider(name)
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
	v := validProvider("codex")
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
	v := validProvider("x")
	v.Protocol = "openai-completions" // not in the closed set
	if err := v.validate("x"); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("unknown protocol must be rejected, got %v", err)
	}
}

func TestValidateWire(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Provider)
		wantErr string // substring; "" = must pass
	}{
		{"openai-chat ok", func(v *Provider) {}, ""},
		{"openai-responses ok", func(v *Provider) { v.Protocol = ProtocolOpenAIResponses }, ""},
		{"openai missing upstream_url", func(v *Provider) { v.UpstreamURL = "" }, "requires upstream_url"},
		{"openai with codex backend", func(v *Provider) { v.SecretBackend = CodexOAuthBackend; v.SecretRef = CodexOAuthBackend }, "not the codex-oauth backend"},
		{"openai non-loopback base", func(v *Provider) { v.BaseURL = "https://api.openai.com/" }, "base_url"},
		{"openai bad upstream", func(v *Provider) { v.UpstreamURL = "http://api.openai.com/v1" }, "upstream_url"},
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
		v := validProvider("c")
		v.Protocol = ProtocolCodexOAuth
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		if err := v.validate("c"); err == nil || !strings.Contains(err.Error(), "requires secret_backend") {
			t.Fatalf("codex protocol needs the codex backend, got %v", err)
		}
	})
	t.Run("codex with upstream_url", func(t *testing.T) {
		v := validProvider("c")
		v.SecretBackend = CodexOAuthBackend
		v.SecretRef = CodexOAuthBackend
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		v.UpstreamURL = "https://api.openai.com/v1"
		if err := v.validate("c"); err == nil || !strings.Contains(err.Error(), "must not set upstream_url") {
			t.Fatalf("codex must reject upstream_url, got %v", err)
		}
	})
	// secret_ref is the per-credential id and becomes a token-file/flock name; a named
	// ref must be a path-safe identifier, an unsafe one is rejected at load.
	codexProvider := func(ref string) *Provider {
		v := validProvider("c")
		v.SecretBackend = CodexOAuthBackend
		v.SecretRef = ref
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		return v
	}
	t.Run("codex named secret_ref ok", func(t *testing.T) {
		if err := codexProvider("codex-work").validate("c"); err != nil {
			t.Fatalf("a path-safe named secret_ref must pass, got %v", err)
		}
	})
	t.Run("codex path-unsafe secret_ref", func(t *testing.T) {
		if err := codexProvider("../../etc/passwd").validate("c"); err == nil || !strings.Contains(err.Error(), "secret_ref") {
			t.Fatalf("a path-unsafe secret_ref must be rejected, got %v", err)
		}
	})

	// Anthropic-native must not carry upstream_url.
	t.Run("anthropic with upstream_url", func(t *testing.T) {
		v := validProvider("a")
		v.UpstreamURL = "https://api.openai.com/v1"
		if err := v.validate("a"); err == nil || !strings.Contains(err.Error(), "only valid for an openai-*") {
			t.Fatalf("anthropic-native must reject upstream_url, got %v", err)
		}
	})
}

// Two codex providers must not resolve to the same login credential: the default
// (sentinel/empty) is single, and named credentials must be distinct.
func TestValidate_CodexCredentialUniqueness(t *testing.T) {
	codex := func(name, ref string) *Provider {
		v := validProvider(name)
		v.Protocol = ProtocolCodexOAuth
		v.SecretBackend = CodexOAuthBackend
		v.SecretRef = ref
		v.BaseURL = "http://127.0.0.1:17222/"
		v.ModelsEndpoint = "http://127.0.0.1:17222/v1/models"
		return v
	}
	cfg := func(vs ...*Provider) *Config {
		c := &Config{Version: SchemaVersion, Providers: map[string]*Provider{}}
		for _, v := range vs {
			c.Providers[v.Name] = v
		}
		return c
	}

	// One default + one named is fine.
	if err := cfg(codex("codex", CodexOAuthBackend), codex("codex-work", "codex-work")).Validate(); err != nil {
		t.Fatalf("distinct codex credentials must pass, got %v", err)
	}
	// Two rows on the default (sentinel) credential → collision.
	if err := cfg(codex("a", CodexOAuthBackend), codex("b", CodexOAuthBackend)).Validate(); err == nil || !strings.Contains(err.Error(), "share credential") {
		t.Fatalf("two default-credential codex rows must be rejected, got %v", err)
	}
	// Two rows with the same named ref → collision.
	if err := cfg(codex("a", "shared"), codex("b", "shared")).Validate(); err == nil || !strings.Contains(err.Error(), "share credential") {
		t.Fatalf("two codex rows sharing a named credential must be rejected, got %v", err)
	}
	// A second codex whose secret_ref is literally the sentinel collapses onto the
	// default credential — must be rejected, not silently share it.
	if err := cfg(codex("first", CodexOAuthBackend), codex("codex-oauth", CodexOAuthBackend)).Validate(); err == nil || !strings.Contains(err.Error(), "share credential") {
		t.Fatalf("a sentinel-named second codex must be rejected, got %v", err)
	}
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
	cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{
		"deepseek": validProvider("deepseek"),
	}}
	p := filepath.Join(t.TempDir(), "providers.toml")
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
