package vendorclass

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

func TestMatchClass(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"clean text", "all tests passed", ""},
		{"balance zh", "API Error: Request rejected (429) 余额不足或无可用资源包", ClassInsufficientBalance},
		{"balance en", "insufficient balance, please recharge", ClassInsufficientBalance},
		{"quota", "You exceeded your current quota", ClassInsufficientBalance},
		{"auth word", "API Error: Unauthorized", ClassAuth},
		{"auth invalid key", "invalid api key provided", ClassAuth},
		{"auth paren 403", "request failed (403)", ClassAuth},
		{"rate phrase", "rate limit exceeded, retrying", ClassRateLimit},
		{"rate 429 paren", "got (429) from upstream", ClassRateLimit},
		{"generic api", "API Error: something unexpected happened", ClassAPIError},
		{"overloaded", "overloaded, please try again", ClassAPIError},
		{"case-insensitive", "API ERROR: RATE LIMIT", ClassRateLimit},
		// Priority ladder: balance must beat auth and rate when several fire.
		{"priority balance over auth", "(401) unauthorized; also 余额不足", ClassInsufficientBalance},
		{"priority balance over rate", "(429) too many requests; 余额已用尽", ClassInsufficientBalance},
		{"priority auth over rate", "(401) unauthorized and (429)", ClassAuth},
		// Bare digits without parens/phrase must NOT trip a class.
		{"bare numbers no match", "compiled 429 files in 401ms", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchClass(tc.in); got != tc.want {
				t.Fatalf("MatchClass(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// vendorFor builds a minimal *config.Vendor pointing its models_endpoint at url.
func vendorFor(url string) *config.Vendor {
	return &config.Vendor{
		Name:           "probe-test",
		BaseURL:        url + "/anthropic",
		DefaultModel:   "m",
		ModelsEndpoint: url + "/v1/models",
		SecretBackend:  "file",
		SecretRef:      "probe-test.key",
		Enabled:        true,
	}
}

func TestReachability(t *testing.T) {
	// Isolate config so secrets.Keyget can't read the dev machine's real
	// vendors.toml. The probe key lookup is best-effort; with no config it
	// degrades to a keyless reachability probe, which is all these httptest
	// servers need (none gate on auth).
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Run("200 no block", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		p := Reachability(vendorFor(srv.URL))
		if p.Block || p.Warn != "" {
			t.Fatalf("200 → Block=%v Warn=%q, want no block, no warn", p.Block, p.Warn)
		}
	})

	t.Run("401 blocks KEY_INVALID", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		p := Reachability(vendorFor(srv.URL))
		if !p.Block || p.Code != "KEY_INVALID" {
			t.Fatalf("401 → Block=%v Code=%q, want block KEY_INVALID", p.Block, p.Code)
		}
	})

	t.Run("403 blocks KEY_INVALID", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		p := Reachability(vendorFor(srv.URL))
		if !p.Block || p.Code != "KEY_INVALID" {
			t.Fatalf("403 → Block=%v Code=%q, want block KEY_INVALID", p.Block, p.Code)
		}
	})

	t.Run("500 warns no block", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		p := Reachability(vendorFor(srv.URL))
		if p.Block {
			t.Fatalf("500 → Block=true, want no block")
		}
		if p.Warn == "" {
			t.Fatalf("500 → empty Warn, want a warning")
		}
	})

	t.Run("bad host blocks VENDOR_UNREACHABLE", func(t *testing.T) {
		// 127.0.0.1:1 (reserved tcpmux) is never listening → dial refused, no
		// HTTP response → transport failure.
		p := Reachability(vendorFor("http://127.0.0.1:1"))
		if !p.Block || p.Code != "VENDOR_UNREACHABLE" {
			t.Fatalf("conn refused → Block=%v Code=%q, want block VENDOR_UNREACHABLE", p.Block, p.Code)
		}
	})

	t.Run("nil vendor no block", func(t *testing.T) {
		if p := Reachability(nil); p.Block {
			t.Fatalf("nil vendor → Block=true, want no block")
		}
	})
}
