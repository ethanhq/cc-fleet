package codexproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

func postShutdownTo(t *testing.T, base, key string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/shutdown", nil)
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

// /shutdown requires the exact handshake secret for every protocol; a wrong or
// missing key is 401, a non-POST is 405, and the matched request drains.
func TestShutdownAuth(t *testing.T) {
	for _, protocol := range []string{config.ProtocolCodexOAuth, config.ProtocolOpenAIChat} {
		drained := make(chan struct{}, 1)
		srv := newServer(&stubUpstream{}, protocol, "topsecret")
		srv.shutdown = func() { drained <- struct{}{} }
		ts := httptest.NewServer(srv.handler())

		if got := postShutdownTo(t, ts.URL, "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("%s wrong secret = %d, want 401", protocol, got)
		}
		if got := postShutdownTo(t, ts.URL, ""); got != http.StatusUnauthorized {
			t.Fatalf("%s missing secret = %d, want 401", protocol, got)
		}
		// A non-POST is rejected before the auth check.
		if r, err := http.Get(ts.URL + "/shutdown"); err != nil {
			t.Fatal(err)
		} else {
			r.Body.Close()
			if r.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s GET /shutdown = %d, want 405", protocol, r.StatusCode)
			}
		}
		if len(drained) != 0 {
			t.Fatalf("%s shutdown must not fire on a refused request", protocol)
		}
		if got := postShutdownTo(t, ts.URL, "topsecret"); got != http.StatusOK {
			t.Fatalf("%s right secret = %d, want 200", protocol, got)
		}
		<-drained // the matched request drains
		ts.Close()
	}
}

// An empty handshake (secret never loaded) refuses every /shutdown — there is no
// "no secret means open" hole.
func TestShutdownEmptyHandshakeRefuses(t *testing.T) {
	srv := newServer(&stubUpstream{}, config.ProtocolOpenAIChat, "")
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	if got := postShutdownTo(t, ts.URL, ""); got != http.StatusUnauthorized {
		t.Fatalf("empty handshake = %d, want 401", got)
	}
}

// stopViaShutdown's primary path: POST /shutdown with the right secret drains and no
// fallback runs; a wrong/empty secret or a non-200 falls through to stopProcess.
func TestStopViaShutdownPrimary(t *testing.T) {
	drained := make(chan struct{}, 1)
	srv := newServer(&stubUpstream{}, config.ProtocolCodexOAuth, "tok")
	srv.shutdown = func() { drained <- struct{}{} }
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	port := serverPort(t, ts)

	// Right secret -> 200, so postShutdown reports success and the daemon drains.
	if !postShutdown(port, "tok") {
		t.Fatal("postShutdown with the right secret must report success")
	}
	<-drained

	// A wrong secret -> 401, so postShutdown reports failure (caller falls back).
	if postShutdown(port, "nope") {
		t.Fatal("postShutdown with a wrong secret must report failure")
	}
}

// stopViaShutdown skips the network when the secret is empty (load-only miss) and
// goes straight to the platform fallback; with a dead pid that fallback is a no-op.
func TestStopViaShutdownEmptySecretIsFallbackOnly(t *testing.T) {
	// pid <= 0 makes stopProcess a no-op on every platform, so this asserts the
	// empty-secret branch takes the fallback without a panic or a network call.
	stopViaShutdown(0, proxyState{Port: 0, PID: 0}, "")
}

// loadSecretOnly reads but never creates the secret file.
func TestLoadSecretOnlyNeverCreates(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome) // windows reads USERPROFILE
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got := loadSecretOnly(); got != "" {
		t.Fatalf("absent secret = %q, want empty", got)
	}
	p, err := secretPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("loadSecretOnly must not create the secret file (stat err=%v)", err)
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("  abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSecretOnly(); got != "abc123" {
		t.Fatalf("loadSecretOnly = %q, want trimmed abc123", got)
	}
}

func serverPort(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
