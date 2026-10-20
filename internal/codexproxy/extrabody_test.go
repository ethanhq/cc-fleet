package codexproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// jsonKeys lists the JSON keys of a request struct, sorted.
func jsonKeys(t reflect.Type) []string {
	var keys []string
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = t.Field(i).Name
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

// The reserved keys live in config (codexproxy imports it, not the reverse), so this
// is what fails when a request struct gains a field and the list is not updated.
func TestExtraBodyReservedKeysMatchRequestStructs(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		req      any
	}{
		{config.ProtocolOpenAIChat, chatRequest{}},
		{config.ProtocolOpenAIResponses, responsesRequest{}},
	} {
		got, want := config.ExtraBodyReservedKeys(tc.protocol), jsonKeys(reflect.TypeOf(tc.req))
		if !slices.Equal(got, want) {
			t.Errorf("%s: reserved keys %v, request struct JSON keys %v", tc.protocol, got, want)
		}
	}
}

func TestDaemonArgs(t *testing.T) {
	const eb = `{"note":"say \"hi\" C:\\x"}`
	cases := []struct {
		name, protocol, upstream, ref, extra string
		tail                                 []string
	}{
		{"no upstream", config.ProtocolCodexOAuth, "", config.CodexOAuthBackend, "", []string{"--protocol", "codex-oauth"}},
		{"upstream", config.ProtocolOpenAIChat, "https://api.openai.com/v1", "", "", []string{"--protocol", "openai-chat", "--upstream-url", "https://api.openai.com/v1"}},
		{"named credential", config.ProtocolCodexOAuth, "", "codex-work", "", []string{"--protocol", "codex-oauth", "--credential", "codex-work"}},
		{"extra_body goes last, value unchanged", config.ProtocolOpenAIChat, "https://x/v1", "", eb, []string{"--protocol", "openai-chat", "--upstream-url", "https://x/v1", "--extra-body", eb}},
	}
	for i, c := range cases {
		port := 17240 + i
		want := append([]string{"codex-proxy", "serve", "--port", strconv.Itoa(port)}, c.tail...)
		if got := daemonArgs(port, c.protocol, c.upstream, c.ref, c.extra, false); !slices.Equal(got, want) {
			t.Errorf("%s: daemonArgs =\n%q\nwant\n%q", c.name, got, want)
		}
	}
}

func TestExtraMembers(t *testing.T) {
	for _, c := range []struct{ name, raw, want string }{
		{"empty string", ``, ``},
		{"empty object", `{}`, ``},
		{"invalid JSON", `{"a":`, ``},
		{"spaced, nested, key-sorted", ` { "zeta": 1,
			"provider": {"only": ["deepseek"], "allow_fallbacks": false} } `,
			`"provider":{"only":["deepseek"],"allow_fallbacks":false},"zeta":1`},
		{"last duplicate wins, null kept", `{"b":null,"a":1,"a":2}`, `"a":2,"b":null`},
	} {
		got := extraMembers(c.raw)
		if string(got) != c.want || (c.want == "" && got != nil) {
			t.Errorf("%s: extraMembers(%q) = %q, want %q (nil when empty)", c.name, c.raw, got, c.want)
		}
	}
}

func TestWithExtra(t *testing.T) {
	const plain = `{"model":"m","stream":true}`
	body := []byte(plain)
	if got := withExtra(body, nil); string(got) != plain {
		t.Fatalf("no members must leave the body untouched, got %s", got)
	}
	if got, want := withExtra(body, []byte(`"provider":{"only":["x"]}`)), `{"model":"m","stream":true,"provider":{"only":["x"]}}`; string(got) != want {
		t.Fatalf("withExtra = %s, want %s", got, want)
	}
	if string(body) != plain {
		t.Fatalf("withExtra modified its input: %s", body)
	}
}

// Each openai-* upstream sends exactly what the converter marshals when extra_body is
// empty or `{}`, and that body with `,<members>` before the closing brace otherwise.
func TestUpstreamsExtraBody(t *testing.T) {
	const raw = ` { "zeta": 1, "provider": {"only": ["deepseek"], "allow_fallbacks": false} } `
	const members = `"provider":{"only":["deepseek"],"allow_fallbacks":false},"zeta":1`
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	a := parseReq(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	chat, _ := translateChatRequest(a, newConvCtx(a, "key"))
	resp, _ := translateRequest(a, newConvCtx(a, "key"))
	resp.MaxOutputTokens = a.MaxTokens
	for protocol, req := range map[string]any{config.ProtocolOpenAIChat: chat, config.ProtocolOpenAIResponses: resp} {
		plain, _ := json.Marshal(req)
		for extra, want := range map[string]string{
			"":   string(plain),
			"{}": string(plain),
			raw:  string(plain[:len(plain)-1]) + "," + members + "}",
		} {
			up, err := buildUpstream(protocol, srv.URL+"/v1", "", extra, false)
			if err != nil {
				t.Fatal(err)
			}
			body, err := up.call(context.Background(), a, newConvCtx(a, "key"))
			if err != nil {
				t.Fatalf("%s call: %v", protocol, err)
			}
			body.Close()
			if string(last) != want || !json.Valid(last) {
				t.Errorf("%s with extra_body %q: body\n%s\nwant valid JSON\n%s", protocol, extra, last, want)
			}
		}
	}
}

func TestBuildUpstreamExtraBody(t *testing.T) {
	for _, c := range []struct{ protocol, raw, wantErr string }{
		{config.ProtocolOpenAIChat, `{"Messages":[]}`, `must not set "messages"`},
		{config.ProtocolOpenAIResponses, `{"input":[]}`, `must not set "input"`},
		{config.ProtocolCodexOAuth, `{"a":1}`, "only valid for an openai-*"},
	} {
		if _, err := buildUpstream(c.protocol, "https://x/v1", "", c.raw, false); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s with extra_body %s: want error containing %q, got %v", c.protocol, c.raw, c.wantErr, err)
		}
	}
}

// healthy treats extra_body as part of the daemon identity: a daemon started with a
// different value (or none) is not reused. An empty value is written without the key,
// like a state file from before the field existed.
func TestHealthyComparesExtraBody(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "cc-fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	start, _ := procintrospect.ProcStart(os.Getpid())
	const up, eb = "https://openrouter.ai/api/v1", `{"provider":{"only":["deepseek"]}}`

	for _, c := range []struct {
		name, started, asked string
		want                 bool
	}{
		{"same", eb, eb, true},
		{"different", eb, `{"provider":{"only":["glm"]}}`, false},
		{"dropped by the provider", eb, "", false},
		{"state file without the field", "", "", true},
		{"gained by the provider", "", eb, false},
	} {
		st := proxyState{Port: port, PID: os.Getpid(), ProcStart: start, Protocol: config.ProtocolOpenAIChat, UpstreamURL: up, ExtraBody: c.started}
		if err := writeState(st); err != nil {
			t.Fatal(err)
		}
		if got := healthy(port, config.ProtocolOpenAIChat, up, "", c.asked, false); got != c.want {
			t.Errorf("%s: healthy = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestProxyStateOmitsEmptyExtraBody(t *testing.T) {
	if b, _ := json.Marshal(proxyState{Port: 1, PID: 2}); strings.Contains(string(b), "extra_body") {
		t.Fatalf("state file gained an extra_body key: %s", b)
	}
}
