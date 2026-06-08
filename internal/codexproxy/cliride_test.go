package codexproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recordRT fails any request and counts it, so a test can assert the ride path
// reaches the network zero times.
type recordRT struct{ calls int }

func (r *recordRT) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("no network expected on the ride path")
}

// writeCLIAuth writes a ~/.codex/auth.json carrying the given access token plus a
// refresh_token that cc-fleet must never read, and returns the exact bytes.
func writeCLIAuth(t *testing.T, home, access string) []byte {
	t.Helper()
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      "idt",
			"access_token":  access,
			"refresh_token": "SECRET-RT-DO-NOT-READ",
			"account_id":    "acc-stored",
		},
		"last_refresh": "2026-06-08T00:00:00Z",
	})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func rideStore(t *testing.T, rec *recordRT) *cliRideStore {
	t.Helper()
	own, err := newOwnStore()
	if err != nil {
		t.Fatal(err)
	}
	own.oauth = &oauthClient{http: &http.Client{Transport: rec}}
	return &cliRideStore{own: own}
}

func authBytes(t *testing.T, home string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An unexpired ~/.codex token is served read-only as a generation-0 bearer
// without any network call, and the auth file is left byte-identical.
func TestCLIRide_ServesUnexpiredTokenReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	access := fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	before := writeCLIAuth(t, home, access)

	rec := &recordRT{}
	b, err := rideStore(t, rec).token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if b.accessToken != access || b.generation != 0 {
		t.Fatalf("ride bearer = {gen %d}, want the CLI access token at gen 0", b.generation)
	}
	if b.accountID != "acc-stored" {
		t.Fatalf("account id = %q, want acc-stored", b.accountID)
	}
	if rec.calls != 0 {
		t.Fatalf("ride path made %d network calls, want 0", rec.calls)
	}
	if !bytes.Equal(before, authBytes(t, home)) {
		t.Fatal("~/.codex/auth.json was modified")
	}
}

// An expired CLI token falls back to the own chain (here absent → ErrReauth),
// and the auth file is still left byte-identical.
func TestCLIRide_FallsBackWhenExpired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	access := fakeJWT(map[string]any{"exp": float64(time.Now().Add(-time.Hour).Unix())})
	before := writeCLIAuth(t, home, access)

	rec := &recordRT{}
	if _, err := rideStore(t, rec).token(context.Background()); !errors.Is(err, ErrReauth) {
		t.Fatalf("expired ride + no own login: err = %v, want ErrReauth", err)
	}
	if !bytes.Equal(before, authBytes(t, home)) {
		t.Fatal("~/.codex/auth.json was modified on the fallback path")
	}
}

// With no ~/.codex at all, token() falls straight through to the own chain.
func TestCLIRide_FallsBackWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recordRT{}
	if _, err := rideStore(t, rec).token(context.Background()); !errors.Is(err, ErrReauth) {
		t.Fatalf("absent ride + no own login: err = %v, want ErrReauth", err)
	}
}

// With both an own login and a valid ride token present, token() serves the own
// login (its bearer is generation != 0; the ride's is 0).
func TestCLIRide_OwnLoginTakesPriority(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeCLIAuth(t, home, fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())}),
			"refresh_token": "rt-next",
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
	if err := os.WriteFile(p, []byte(`{"refresh_token":"rt-0","account_id":"own-acc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	own, err := newOwnStore()
	if err != nil {
		t.Fatal(err)
	}
	own.oauth = &oauthClient{http: &http.Client{Transport: rewriteRT{target}}}
	s := &cliRideStore{own: own}

	b, err := s.token(context.Background())
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if b.generation == 0 {
		t.Fatal("own login must take priority over the ride (own bearer is gen != 0)")
	}
}

// invalidate(0) is a no-op (the CLI-ride token cannot be refreshed); a non-zero
// generation forwards to the own chain.
func TestCLIRide_InvalidateGenDomain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	own, err := newOwnStore()
	if err != nil {
		t.Fatal(err)
	}
	own.gen = 5
	own.expiry = time.Now().Add(time.Hour)
	s := &cliRideStore{own: own}

	s.invalidate(0)
	if own.expiry.IsZero() {
		t.Fatal("invalidate(0) must not disturb the own chain")
	}
	s.invalidate(5)
	if !own.expiry.IsZero() {
		t.Fatal("invalidate(own gen) must force the own chain to refresh")
	}
}

func TestStatusReport(t *testing.T) {
	unexpired := func() string {
		return fakeJWT(map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	}
	writeOwn := func(t *testing.T) {
		t.Helper()
		p, err := storePath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"refresh_token":"rt","account_id":"own-acc"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("none", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		st := StatusReport()
		if st.CLIRide || st.OwnLogin || st.Active != "none" {
			t.Fatalf("none: %+v", st)
		}
	})
	t.Run("ride only", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeCLIAuth(t, home, unexpired())
		st := StatusReport()
		if !st.CLIRide || st.OwnLogin || st.Active != "cli-ride" || st.Account == "" {
			t.Fatalf("ride only: %+v", st)
		}
	})
	t.Run("own only", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeOwn(t)
		st := StatusReport()
		if st.CLIRide || !st.OwnLogin || st.Active != "own" {
			t.Fatalf("own only: %+v", st)
		}
	})
	t.Run("both prefer own", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		writeCLIAuth(t, home, unexpired())
		writeOwn(t)
		st := StatusReport()
		if !st.CLIRide || !st.OwnLogin || st.Active != "own" {
			t.Fatalf("both: %+v", st)
		}
	})
}

func TestAccountIDOrganizationsRung(t *testing.T) {
	// organizations[0].id is the last-resort rung under the auth namespace.
	orgClaim := map[string]any{jwtAuthClaim: map[string]any{
		"organizations": []any{map[string]any{"id": "org-7"}},
	}}
	if got := accountIDFromTokens(&tokens{IDToken: fakeJWT(orgClaim)}); got != "org-7" {
		t.Fatalf("organizations rung: %q", got)
	}
	// a top-level chatgpt_account_id wins over the namespaced claim.
	top := map[string]any{
		"chatgpt_account_id": "top-1",
		jwtAuthClaim:         map[string]any{"chatgpt_account_id": "nested"},
	}
	if got := accountIDFromTokens(&tokens{AccessToken: fakeJWT(top)}); got != "top-1" {
		t.Fatalf("top-level account id: %q", got)
	}
}
