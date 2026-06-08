package spawn

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
)

// codexStanza returns a vendors.toml stanza for a codex provider whose
// endpoints point at base.
func codexStanza(base string) string {
	return fmt.Sprintf(`
[codex]
base_url        = "%s/"
default_model   = "gpt-5.5"
models_endpoint = "%s/v1/models"
secret_backend  = "codex-oauth"
secret_ref      = "codex-oauth"
enabled         = true
added_at        = 2026-06-08T05:00:00Z
`, base, base)
}

// A codex provider's models endpoint is served by the lazily-started daemon,
// so the step-4 probe is skipped (it would always fail before 5c starts the
// daemon); daemon readiness at 5c is the health signal instead.
func TestSpawn_CodexSkipsProbe_EnsuresDaemon(t *testing.T) {
	f := newFixture(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)
	f.writeVendorsTOML(codexStanza(srv.URL))
	f.writeFingerprint()

	var ensured atomic.Int32
	ensureVendorProxy = func(v *config.Vendor) error {
		if v == nil || v.SecretBackend != codexproxy.SecretBackend {
			t.Errorf("ensureVendorProxy got a non-codex vendor: %+v", v)
		}
		ensured.Add(1)
		return nil
	}
	t.Cleanup(func() { ensureVendorProxy = codexproxy.EnsureForVendor })

	res := Spawn(Request{Vendor: "codex", AgentName: "cw", Team: "ct", Probe: true, AutoTeam: true})
	if !res.OK {
		t.Fatalf("codex spawn: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("codex probe must be skipped; models endpoint hit %d time(s)", got)
	}
	if got := ensured.Load(); got != 1 {
		t.Fatalf("ensureVendorProxy calls = %d, want 1", got)
	}
}

// A daemon-ensure failure is fail-before-mutation: classified result, no
// profile written, no team dir, no tmux call.
func TestSpawn_CodexDaemonFailure_FailsBeforeMutation(t *testing.T) {
	f := newFixture(t)
	f.writeVendorsTOML(codexStanza("http://127.0.0.1:17222"))
	f.writeFingerprint()

	ensureVendorProxy = func(*config.Vendor) error {
		return fmt.Errorf("codex proxy did not become ready on port 17222")
	}
	t.Cleanup(func() { ensureVendorProxy = codexproxy.EnsureForVendor })

	res := Spawn(Request{Vendor: "codex", AgentName: "cw", Team: "ct2", Probe: false, AutoTeam: true})
	if res.OK || res.ErrorCode != ErrCodeProxyUnavailable {
		t.Fatalf("want CODEX_PROXY_UNAVAILABLE, got ok=%v code=%s", res.OK, res.ErrorCode)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".claude", "profiles", "codex.json")); !os.IsNotExist(err) {
		t.Fatalf("profile must not be written on daemon failure (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".claude", "teams", "ct2")); !os.IsNotExist(err) {
		t.Fatalf("team dir must not be created on daemon failure")
	}
	if calls := f.readMockArgs(); len(calls) != 0 {
		t.Fatalf("no tmux calls expected on daemon failure, got %v", calls)
	}
}
