package codexproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// rewriteRT redirects every request to the test server, keeping the path.
type rewriteRT struct{ target *url.URL }

func (rt rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.target.Scheme
	req.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(req)
}

// Two stores (simulating the login CLI and the daemon) sharing one on-disk
// chain: the second refresh must reload the rotated token from disk under the
// token lock — presenting the superseded one trips reuse detection (401).
func TestOwnStore_RefreshReloadsRotatedChainFromDisk(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	var mu sync.Mutex
	valid := map[string]bool{"rt-0": true}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rt := r.PostFormValue("refresh_token")
		mu.Lock()
		defer mu.Unlock()
		if !valid[rt] {
			w.WriteHeader(http.StatusUnauthorized) // reuse detection
			return
		}
		delete(valid, rt)
		n++
		next := fmt.Sprintf("rt-%d", n)
		valid[next] = true
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())}),
			"refresh_token": next,
		})
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)

	p, err := storePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"refresh_token":"rt-0","account_id":"acc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mk := func() *ownStore {
		s, err := newOwnStore()
		if err != nil {
			t.Fatal(err)
		}
		s.oauth = &oauthClient{http: &http.Client{Transport: rewriteRT{target}}}
		return s
	}
	a, b := mk(), mk() // both loaded rt-0 into memory

	if _, err := a.token(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	// b still holds rt-0 in memory; without the on-disk double-check this
	// presents a consumed token and dies with ErrReauth.
	if _, err := b.token(context.Background()); err != nil {
		t.Fatalf("second store's refresh must reload the rotated chain: %v", err)
	}

	disk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(disk), "rt-2") {
		t.Fatalf("disk chain not at the latest rotation: %s", disk)
	}
}
