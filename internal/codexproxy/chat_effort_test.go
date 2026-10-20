package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

func TestChatEffortField(t *testing.T) {
	for _, c := range []struct{ name, upstream, extra, want string }{
		{"openrouter", "https://openrouter.ai/api/v1", "", "reasoning"},
		{"openrouter subdomain", "https://eu.openrouter.ai/api/v1", "", "reasoning"},
		{"lookalike host", "https://notopenrouter.ai/v1", "", "reasoning_effort"},
		{"other endpoint", "https://api.deepseek.com/v1", "", "reasoning_effort"},
		{"extra_body sets reasoning", "https://openrouter.ai/api/v1", `{"reasoning":{"effort":"low"}}`, ""},
		{"extra_body sets reasoning_effort, any case", "https://api.deepseek.com/v1", `{"Reasoning_Effort":"low"}`, ""},
		{"unrelated extra_body", "https://openrouter.ai/api/v1", `{"provider":{"only":["deepseek"]}}`, "reasoning"},
	} {
		if got := chatEffortField(c.upstream, c.extra); got != c.want {
			t.Errorf("%s: chatEffortField = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestChatEffortMember(t *testing.T) {
	const adaptive = `"thinking":{"type":"adaptive"}`
	for _, c := range []struct{ name, req, field, want string }{
		{"openrouter keeps max", `{"model":"m",` + adaptive + `,"output_config":{"effort":"max"}}`, "reasoning", `"reasoning":{"effort":"max"}`},
		{"openrouter low", `{"model":"m",` + adaptive + `,"output_config":{"effort":"low"}}`, "reasoning", `"reasoning":{"effort":"low"}`},
		{"reasoning_effort clamps max", `{"model":"m",` + adaptive + `,"output_config":{"effort":"max"}}`, "reasoning_effort", `"reasoning_effort":"xhigh"`},
		{"legacy budget", `{"model":"m","thinking":{"type":"enabled","budget_tokens":4000}}`, "reasoning_effort", `"reasoning_effort":"medium"`},
		{"no thinking (slim subagent)", `{"model":"m","output_config":{"effort":"high"}}`, "reasoning", `"reasoning":{"effort":"high"}`},
		{"thinking disabled", `{"model":"m","thinking":{"type":"disabled"},"output_config":{"effort":"low"}}`, "reasoning_effort", `"reasoning_effort":"low"`},
		{"no effort", `{"model":"m",` + adaptive + `}`, "reasoning", ``},
		{"forwarding off", `{"model":"m",` + adaptive + `,"output_config":{"effort":"high"}}`, "", ``},
	} {
		if got := string(chatEffortMember(parseReq(t, c.req), c.field)); got != c.want {
			t.Errorf("%s: member = %s, want %s", c.name, got, c.want)
		}
	}
}

// The chat upstream sends the effort only when forwarding is on, after extra_body's
// members, and never alongside an extra_body that sets the field itself.
func TestUpstreamChatEffort(t *testing.T) {
	var (
		raw  []byte
		last map[string]json.RawMessage
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw, last = b, nil
		if err := json.Unmarshal(b, &last); err != nil {
			t.Errorf("upstream body is not one JSON object: %s", b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	a := parseReq(t, `{"model":"m","max_tokens":10,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`)

	for _, c := range []struct {
		name, extra string
		forward     bool
		want        map[string]string // reasoning-ish key → raw JSON value; others must be absent
	}{
		{"off", "", false, map[string]string{}},
		{"on", "", true, map[string]string{"reasoning_effort": `"low"`}},
		{"on with unrelated extra_body", `{"provider":{"only":["x"]}}`, true, map[string]string{"reasoning_effort": `"low"`}},
		{"extra_body wins", `{"reasoning_effort":"high"}`, true, map[string]string{"reasoning_effort": `"high"`}},
	} {
		up, err := buildUpstream(config.ProtocolOpenAIChat, srv.URL+"/v1", "", c.extra, c.forward)
		if err != nil {
			t.Fatal(err)
		}
		body, err := up.call(context.Background(), a, newConvCtx(a, "key"))
		if err != nil {
			t.Fatalf("%s: call: %v", c.name, err)
		}
		body.Close()
		for _, k := range []string{"reasoning", "reasoning_effort"} {
			if got, want := string(last[k]), c.want[k]; got != want {
				t.Errorf("%s: %s = %s, want %s", c.name, k, got, want)
			}
			if n := bytes.Count(raw, []byte(`"`+k+`"`)); n > 1 {
				t.Errorf("%s: %s appears %d times in %s", c.name, k, n, raw)
			}
		}
		var extra map[string]json.RawMessage
		_ = json.Unmarshal([]byte(c.extra), &extra) // "" leaves it empty
		for k, v := range extra {
			if string(last[k]) != string(v) {
				t.Errorf("%s: extra_body %s = %s upstream, want %s", c.name, k, last[k], v)
			}
		}
	}
}

func TestDaemonForwardEffort(t *testing.T) {
	got := daemonArgs(17250, config.ProtocolOpenAIChat, "https://x/v1", "", "", true)
	want := []string{"codex-proxy", "serve", "--port", "17250", "--protocol", "openai-chat", "--upstream-url", "https://x/v1", "--forward-effort"}
	if !slices.Equal(got, want) {
		t.Fatalf("daemonArgs =\n%q\nwant\n%q", got, want)
	}

	// Forwarding is part of the daemon identity, so turning it on or off restarts the daemon.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "cc-fleet"), 0o700); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	start, _ := procintrospect.ProcStart(os.Getpid())
	for _, c := range []struct {
		name           string
		started, asked bool
		want           bool
	}{
		{"same, on", true, true, true},
		{"same, off", false, false, true},
		{"turned on", false, true, false},
		{"turned off", true, false, false},
	} {
		st := proxyState{Port: port, PID: os.Getpid(), ProcStart: start, Protocol: config.ProtocolOpenAIChat, UpstreamURL: "https://x/v1", ForwardEffort: c.started}
		if err := writeState(st); err != nil {
			t.Fatal(err)
		}
		if got := healthy(port, config.ProtocolOpenAIChat, "https://x/v1", "", "", c.asked); got != c.want {
			t.Errorf("%s: healthy = %v, want %v", c.name, got, c.want)
		}
	}
}
