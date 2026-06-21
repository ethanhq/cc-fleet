package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestCheckAssetRedirect: the redirect policy allows https within the hop cap, refuses an
// http downgrade, and stops a loop past the cap (a non-nil CheckRedirect drops Go's default).
func TestCheckAssetRedirect(t *testing.T) {
	httpsReq := &http.Request{URL: mustURL(t, "https://cdn.example/x")}
	if err := checkAssetRedirect(httpsReq, make([]*http.Request, 3)); err != nil {
		t.Errorf("https within the cap should be allowed: %v", err)
	}
	if err := checkAssetRedirect(&http.Request{URL: mustURL(t, "http://evil.example/x")}, nil); err == nil {
		t.Error("an http downgrade redirect should be refused")
	}
	if err := checkAssetRedirect(httpsReq, make([]*http.Request, 10)); err == nil {
		t.Error("a redirect past the 10-hop cap should be stopped")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestAssetBase_RequiresHTTPS: the CCF_BASE_URL override must be https — #68.
func TestAssetBase_RequiresHTTPS(t *testing.T) {
	t.Setenv("CCF_BASE_URL", "")
	got, err := assetBase("v1.2.3")
	if err != nil || !strings.HasPrefix(got, githubBase+"/") {
		t.Fatalf("default base: got %q err %v", got, err)
	}

	t.Setenv("CCF_BASE_URL", "https://mirror.example/dl")
	if got, err := assetBase("v1.2.3"); err != nil || got != "https://mirror.example/dl" {
		t.Fatalf("https override: got %q err %v", got, err)
	}

	for _, bad := range []string{"http://evil.example", "ftp://x", "evil.example", "HtTp://x"} {
		t.Setenv("CCF_BASE_URL", bad)
		if _, err := assetBase("v1.2.3"); err == nil {
			t.Errorf("CCF_BASE_URL=%q should be rejected", bad)
		}
	}
}

// TestDownload_RejectsInsecureRedirect: the asset download follows https-only redirects;
// an https→http downgrade fails closed, while a direct (un-redirected) fetch still works — #67.
func TestDownload_RejectsInsecureRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "http://downgrade.example/x", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if body, err := download(context.Background(), srv.URL+"/ok"); err != nil || string(body) != "ok" {
		t.Fatalf("direct GET: body %q err %v", body, err)
	}
	if _, err := download(context.Background(), srv.URL+"/redir"); err == nil {
		t.Error("a redirect to http should be refused")
	}
}
