package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Newline, quote, backslash, JSON escapes and non-ASCII: must survive TOML escaping and the JSON check.
const awkwardExtraBody = "{\n  \"note\": \"quote \\\" backslash \\\\ newline \\n\",\n  \"label\": \"héllo 世界 😀\"\n}"

// A valid openai-chat provider row (the fields validate requires), for tests to append an extra_body line to.
const oaiRow = `base_url = "http://127.0.0.1:17240/"
default_model = "m"
models_endpoint = "https://api.openai.com/v1/models"
secret_backend = "file"
secret_ref = "oai.key"
protocol = "openai-chat"
upstream_url = "https://api.openai.com/v1"`

// Through providers.toml and through a bundle; an unset value leaves no extra_body key in the file.
func TestExtraBodyRoundTrip(t *testing.T) {
	v := validOpenAIChat("oai")
	cfg := &Config{Version: SchemaVersion, Providers: map[string]*Provider{"oai": v}}
	path := filepath.Join(t.TempDir(), "providers.toml")
	for _, eb := range []string{awkwardExtraBody, ""} {
		v.ExtraBody = eb
		if err := SaveToPath(cfg, path); err != nil {
			t.Fatal(err)
		}
		raw, errRead := os.ReadFile(path)
		got, errLoad := LoadFromPath(path)
		if errRead != nil || errLoad != nil {
			t.Fatal(errRead, errLoad)
		}
		if g := got.Providers["oai"].ExtraBody; g != eb {
			t.Fatalf("after save+load extra_body = %q, want %q", g, eb)
		}
		if has := strings.Contains(string(raw), "extra_body"); has != (eb != "") {
			t.Fatalf("extra_body key in file = %v for value %q", has, eb)
		}
		data, errMarshal := MarshalBundle(&Bundle{Version: BundleVersion, Providers: cfg.Providers})
		b, errParse := ParseBundle(data)
		if errMarshal != nil || errParse != nil {
			t.Fatal(errMarshal, errParse)
		}
		if g := b.Providers["oai"].ExtraBody; g != eb {
			t.Fatalf("after bundle export+parse extra_body = %q, want %q", g, eb)
		}
	}
}

func TestValidateExtraBody(t *testing.T) {
	cases := []struct{ name, protocol, raw, wantErr string }{ // wantErr: substring; "" = must pass
		{"empty, native", "", "", ""},
		{"empty, chat", ProtocolOpenAIChat, "", ""},
		{"empty, responses", ProtocolOpenAIResponses, "", ""},
		{"empty, codex", ProtocolCodexOAuth, "", ""},
		{"chat object", ProtocolOpenAIChat, `{"provider":{"only":["deepseek"]}}`, ""},
		{"responses object", ProtocolOpenAIResponses, `{"service_tier":"flex"}`, ""},
		{"empty object in whitespace", ProtocolOpenAIChat, " \n{}\n ", ""},
		{"native", "", `{"a":1}`, "extra_body is only valid for an openai-* protocol"},
		{"codex", ProtocolCodexOAuth, `{"a":1}`, "extra_body is only valid for an openai-* protocol"},
		{"invalid utf-8", ProtocolOpenAIChat, "{\"a\":\"\xff\"}", "extra_body must be valid UTF-8"},
		{"exactly the limit", ProtocolOpenAIChat, `{"a":"` + strings.Repeat("x", MaxExtraBodyBytes-8) + `"}`, ""},
		{"one byte over", ProtocolOpenAIChat, `{"a":"` + strings.Repeat("x", MaxExtraBodyBytes-7) + `"}`, "over the 4096-byte limit"},
		{"not json", ProtocolOpenAIChat, `{"a":`, "extra_body must be a JSON object"},
		{"array", ProtocolOpenAIChat, `[1,2]`, "extra_body must be a JSON object"},
		{"null", ProtocolOpenAIChat, `null`, "(not an object)"},
		{"trailing content", ProtocolOpenAIChat, `{"a":1} {"b":2}`, "extra_body must be a JSON object"},
		{"reserved, chat", ProtocolOpenAIChat, `{"tool_choice":1}`, `extra_body must not set "tool_choice": the proxy sets it`},
		{"reserved, other case", ProtocolOpenAIResponses, `{"Store":true}`, `extra_body must not set "store": the proxy sets it`},
		{"several reserved: the smallest is named", ProtocolOpenAIChat, `{"tools":1,"model":2,"messages":3}`, `extra_body must not set "messages"`},
		{"reserved key nested", ProtocolOpenAIChat, `{"provider":{"model":"x"}}`, ""},
		{"responses-only key on chat", ProtocolOpenAIChat, `{"input":[]}`, ""},
		{"chat-only key on responses", ProtocolOpenAIResponses, `{"max_tokens":9}`, ""},
	}
	for _, c := range cases {
		err := ValidateExtraBody(c.protocol, c.raw)
		if (c.wantErr == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.wantErr)
		}
	}
}

func TestExtraBodyReservedKeys(t *testing.T) {
	for _, p := range []string{"", ProtocolCodexOAuth} {
		if k := ExtraBodyReservedKeys(p); k != nil {
			t.Fatalf("protocol %q: got %v, want nil", p, k)
		}
	}
	for _, p := range []string{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		k := ExtraBodyReservedKeys(p)
		ok := len(k) > 0
		for i := 1; i < len(k); i++ {
			ok = ok && k[i-1] < k[i]
		}
		if !ok {
			t.Fatalf("%s: reserved keys %v: want a non-empty, sorted, unique list", p, k)
		}
	}
}

// A reserved key is refused when loading a hand-edited providers.toml and when saving a config.
func TestExtraBodyReservedKeyRejectedAtLoadAndSave(t *testing.T) {
	const want = `config: provider "oai": extra_body must not set "model": the proxy sets it`
	dir := t.TempDir()
	in := filepath.Join(dir, "in.toml")
	if err := os.WriteFile(in, []byte("version = 1\n\n[oai]\n"+oaiRow+"\nextra_body = '{\"model\":\"x\"}'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFromPath(in); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("load: got %v, want error containing %q", err, want)
	}
	v := validOpenAIChat("oai")
	v.ExtraBody = `{"model":"x"}`
	out := filepath.Join(dir, "out.toml")
	if err := SaveToPath(&Config{Version: SchemaVersion, Providers: map[string]*Provider{"oai": v}}, out); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("save: got %v, want error containing %q", err, want)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("an invalid config was written to disk")
	}
	// Each provider is checked against its own protocol: an Anthropic-native one takes none.
	n := validProvider("ds")
	n.ExtraBody = `{"a":1}`
	if err := (&Config{Version: SchemaVersion, Providers: map[string]*Provider{"ds": n}}).Validate(); err == nil ||
		!strings.Contains(err.Error(), `provider "ds": extra_body is only valid for an openai-* protocol`) {
		t.Fatalf("native provider with an extra_body: got %v", err)
	}
}

// extra_body written as a TOML table (or any non-string) gets one clear message from load and from ParseBundle.
func TestExtraBodyWrittenAsTOMLTable(t *testing.T) {
	const want = `config: provider "oai": extra_body must be a string holding a JSON object, for example extra_body = '{"provider":{"only":["deepseek"]}}'`
	for _, tail := range []string{`extra_body = { provider = { only = ["deepseek"] } }`, `extra_body = 5`} {
		_, err := parse([]byte("version = 1\n\n[oai]\n" + oaiRow + "\n" + tail + "\n"))
		if err == nil || err.Error() != want {
			t.Errorf("parse(%s): got %v, want %q", tail, err, want)
		}
		_, err = ParseBundle([]byte("bundle_version = 1\n\n[oai]\n" + oaiRow + "\n" + tail + "\n"))
		if err == nil || err.Error() != want {
			t.Errorf("ParseBundle(%s): got %v, want %q", tail, err, want)
		}
	}
}
