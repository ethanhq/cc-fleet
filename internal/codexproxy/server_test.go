package codexproxy

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// stubUpstream records the apiKey handed to call and emits a single terminal
// event, so a handler test can assert auth + key forwarding without a backend.
type stubUpstream struct {
	mu     sync.Mutex
	gotKey string
}

func (s *stubUpstream) call(_ context.Context, _ *anthropicRequest, cc *convCtx) (io.ReadCloser, error) {
	s.mu.Lock()
	s.gotKey = cc.apiKey
	s.mu.Unlock()
	return io.NopCloser(strings.NewReader("")), nil
}

func (s *stubUpstream) convert(_ io.Reader, sink sseSink, _ *convCtx) error {
	return sink.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *stubUpstream) models() []string { return []string{"stub-1"} }

func postMessages(t *testing.T, base, key string) int {
	t.Helper()
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/messages", strings.NewReader(body))
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// codex gates on the handshake secret and uses no upstream key; openai-* forwards
// the presented real key (b1) and rejects an empty one.
func TestServerAuthPerProtocol(t *testing.T) {
	cu := &stubUpstream{}
	codex := httptest.NewServer(newServer(cu, config.ProtocolCodexOAuth, "secret").handler())
	defer codex.Close()
	if got := postMessages(t, codex.URL, "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("codex wrong handshake = %d, want 401", got)
	}
	if got := postMessages(t, codex.URL, "secret"); got != http.StatusOK {
		t.Fatalf("codex right handshake = %d, want 200", got)
	}
	if cu.gotKey != "" {
		t.Fatalf("codex must not forward an upstream key, got %q", cu.gotKey)
	}

	ou := &stubUpstream{}
	openai := httptest.NewServer(newServer(ou, config.ProtocolOpenAIChat, "").handler())
	defer openai.Close()
	if got := postMessages(t, openai.URL, ""); got != http.StatusUnauthorized {
		t.Fatalf("openai empty key = %d, want 401", got)
	}
	if got := postMessages(t, openai.URL, "sk-real"); got != http.StatusOK {
		t.Fatalf("openai real key = %d, want 200", got)
	}
	if ou.gotKey != "sk-real" {
		t.Fatalf("openai must forward the real key (b1), got %q", ou.gotKey)
	}
}

func TestServerHealthzAndModels(t *testing.T) {
	s := httptest.NewServer(newServer(&stubUpstream{}, config.ProtocolCodexOAuth, "secret").handler())
	defer s.Close()

	r, err := http.Get(s.URL + "/healthz")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %v / %d", err, r.StatusCode)
	}
	r.Body.Close()

	r, err = http.Get(s.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(r.Body).Decode(&doc) != nil || len(doc.Data) != 1 || doc.Data[0].ID != "stub-1" {
		t.Fatalf("/v1/models = %+v", doc)
	}
}

// A live daemon is reused only when its persisted identity matches the requested
// (protocol, upstream_url); a mismatch is not reused.
func TestHealthyReusesOnlyOnIdentityMatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	hs := &http.Server{Handler: mux}
	go hs.Serve(ln)
	defer hs.Close()
	for i := 0; i < 50 && !portResponds(port); i++ {
		time.Sleep(10 * time.Millisecond)
	}

	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start, _ := procintrospect.ProcStart(os.Getpid())
	if err := writeState(proxyState{Port: port, PID: os.Getpid(), ProcStart: start, Protocol: config.ProtocolCodexOAuth}); err != nil {
		t.Fatal(err)
	}

	if !healthy(port, config.ProtocolCodexOAuth, "", "") {
		t.Fatal("matching identity must be reused")
	}
	if healthy(port, config.ProtocolOpenAIChat, "", "") {
		t.Fatal("a different protocol must not be reused")
	}
	if healthy(port, config.ProtocolCodexOAuth, "https://api.openai.com/v1", "") {
		t.Fatal("a different upstream_url must not be reused")
	}
	// A default-credential daemon must not be reused by a named credential.
	if healthy(port, config.ProtocolCodexOAuth, "", "codex-work") {
		t.Fatal("a different codex credential must not be reused")
	}
}
