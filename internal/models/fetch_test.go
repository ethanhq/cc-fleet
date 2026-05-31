package models

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// fakeKey is the API key we install in the test secrets/ dir. We assert it
// shows up in the Authorization header of recorded requests AND that it
// never appears in any error string (even when the vendor echoes it).
const fakeKey = "sk-test-deepseek-XYZ-secret-deadbeef"

// installVendor writes a vendors.toml + secret file pointing at endpointURL
// using the file:// secrets backend so secrets.Keyget returns fakeKey when
// Fetch asks for it. Returns the *config.Vendor that callers pass to Fetch.
func installVendor(t *testing.T, name, endpointURL string) *config.Vendor {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", filepath.Join(xdg, "fakehome"))

	v := &config.Vendor{
		Name:           name,
		BaseURL:        "https://" + name + ".example.com/anthropic",
		DefaultModel:   name + "-latest",
		ModelsEndpoint: endpointURL,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        true,
		AddedAt:        time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC),
	}
	cfg := &config.Config{
		Version: config.SchemaVersion,
		Vendors: map[string]*config.Vendor{name: v},
	}
	cfgPath, err := config.VendorsPath()
	if err != nil {
		t.Fatalf("VendorsPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := config.SaveToPath(cfg, cfgPath); err != nil {
		t.Fatalf("SaveToPath: %v", err)
	}

	secretsDir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, v.SecretRef), []byte(fakeKey), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return v
}

func TestFetch_OpenAIStyle_OK(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
            {"id":"deepseek-v4-flash","owned_by":"deepseek"},
            {"id":"deepseek-v4-pro","owned_by":"deepseek"}
        ]}`)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	got, err := Fetch(context.Background(), v)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := []Model{
		{ID: "deepseek-v4-flash", OwnedBy: "deepseek"},
		{ID: "deepseek-v4-pro", OwnedBy: "deepseek"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("model[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if want := "Bearer " + fakeKey; seenAuth != want {
		t.Fatalf("Authorization header = %q, want %q", seenAuth, want)
	}
}

func TestFetch_AnthropicStyle_DataEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Anthropic's /v1/models returns data[] but with display_name
		// instead of owned_by. We accept both shapes.
		fmt.Fprint(w, `{"data":[
            {"id":"kimi-k2","display_name":"Kimi K2"},
            {"id":"kimi-latest","display_name":"Kimi Latest"}
        ]}`)
	}))
	defer srv.Close()

	v := installVendor(t, "kimi", srv.URL)

	got, err := Fetch(context.Background(), v)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d models, want 2 (%+v)", len(got), got)
	}
	if got[0].ID != "kimi-k2" || got[0].OwnedBy != "Kimi K2" {
		t.Fatalf("model[0] = %+v, want id=kimi-k2 owned_by=Kimi K2", got[0])
	}
}

func TestFetch_AnthropicStyle_BareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":"glm-4.6","display_name":"GLM 4.6"}]`)
	}))
	defer srv.Close()

	v := installVendor(t, "glm", srv.URL)

	got, err := Fetch(context.Background(), v)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 || got[0].ID != "glm-4.6" {
		t.Fatalf("got %+v, want one model with id=glm-4.6", got)
	}
}

func TestFetch_HTTP401_ReturnsErrKeyInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Some vendors echo the bearer key back in 401 bodies — we must
		// neither leak it into the error nor swallow the sentinel.
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"Invalid key %s, please rotate"}`, fakeKey)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch: want error, got nil")
	}
	if !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("err = %v, want wrapped ErrKeyInvalid", err)
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("error message echoed the API key: %q", err.Error())
	}
}

func TestFetch_HTTP500_PlainError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"oops"}`)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch: want error, got nil")
	}
	if errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("err = %v, want NOT ErrKeyInvalid", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %q should mention status 500", err.Error())
	}
}

// TestFetch_HTTP500_ErrorOmitsRawBody: the default Error() string MUST be
// canonical: status + endpoint only, never raw body bytes. A vendor that echoes
// its bearer key into the 5xx body must not leak through Error() / refresh --json.
func TestFetch_HTTP500_ErrorOmitsRawBody(t *testing.T) {
	// Sentinel placed at byte 0 of the body — the strictest form of the bug
	// (truncation can't possibly save us). The masker has to catch it.
	const sentinel = "sk-SENTINEL01234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `%s ...rest of body...`, sentinel)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch: want error, got nil")
	}
	msg := err.Error()
	// Canonical shape: must mention status + endpoint, must NOT contain body.
	if !strings.Contains(msg, "500") {
		t.Fatalf("error %q should mention status 500", msg)
	}
	if strings.Contains(msg, sentinel) {
		t.Fatalf("error %q leaked the byte-0 sentinel — Error() must be canonical", msg)
	}
	if strings.Contains(msg, "...rest of body...") {
		t.Fatalf("error %q leaked raw body — Error() must not include body text", msg)
	}

	// BodyPreview() is the human-debug-only escape hatch; verify it is also
	// masked (defense-in-depth in case a future caller forwards the preview).
	var hse *HTTPStatusError
	if !errors.As(err, &hse) {
		t.Fatalf("err = %v, want *HTTPStatusError", err)
	}
	prev := hse.BodyPreview()
	if strings.Contains(prev, "SENTINEL01234567890") {
		t.Fatalf("BodyPreview() leaked sentinel: %q", prev)
	}
	if !strings.Contains(prev, "[REDACTED]") {
		t.Fatalf("BodyPreview() should show redaction marker, got %q", prev)
	}
}

// TestFetch_HTTP400_ErrorOmitsRawBody mirrors the 5xx case for any 4xx that
// isn't 401 (which has its own KEY_INVALID path).
func TestFetch_HTTP400_ErrorOmitsRawBody(t *testing.T) {
	const sentinel = "Bearer sk-SENTINEL01234567890"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Sentinel near byte 0, then filler; canonical Error() must drop it.
		fmt.Fprintf(w, `%s %s`, sentinel, strings.Repeat("A", 500))
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch: want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "400") {
		t.Fatalf("error %q should mention status 400", msg)
	}
	if strings.Contains(msg, "SENTINEL01234567890") {
		t.Fatalf("error %q leaked sentinel — Error() must be canonical", msg)
	}
	// BodyPreview() is the human-debug-only path; verify masking.
	var hse *HTTPStatusError
	if errors.As(err, &hse) {
		if strings.Contains(hse.BodyPreview(), "SENTINEL01234567890") {
			t.Fatalf("BodyPreview() leaked sentinel: %q", hse.BodyPreview())
		}
	}
}

func TestFetch_ParseError_MentionsEndpoint_NotKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Not an array, not {"data": [...]}.
		fmt.Fprint(w, `{"unexpected":"shape"}`)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch: want parse error, got nil")
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("error %q should include endpoint URL", err.Error())
	}
	if strings.Contains(err.Error(), fakeKey) {
		t.Fatalf("error %q must not contain the API key", err.Error())
	}
}

func TestFetch_ContextTimeout(t *testing.T) {
	// Server hangs forever; we cancel via a short context to force the
	// http.Client to surface context.DeadlineExceeded.
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := Fetch(ctx, v)
	if err == nil {
		t.Fatalf("Fetch: want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want wrapping context.DeadlineExceeded", err)
	}
}

func TestFetch_NilVendor(t *testing.T) {
	_, err := Fetch(context.Background(), nil)
	if err == nil {
		t.Fatalf("Fetch(nil): want error, got nil")
	}
}

func TestFetch_EmptyEndpoint(t *testing.T) {
	v := &config.Vendor{Name: "x"}
	_, err := Fetch(context.Background(), v)
	if err == nil {
		t.Fatalf("Fetch(empty endpoint): want error, got nil")
	}
	if !strings.Contains(err.Error(), "models_endpoint") {
		t.Fatalf("error %q should mention models_endpoint", err.Error())
	}
}

// ----- FetchWithKey (the secrets-free core reused by the spawn probe + the
// TUI add wizard, where the key is held in memory, not on disk) -----

func TestFetchWithKey_SendsKeyAndParses(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"data":[{"id":"m1","owned_by":"x"}]}`)
	}))
	defer srv.Close()

	got, err := FetchWithKey(context.Background(), srv.URL, []byte("inline-key-123"))
	if err != nil {
		t.Fatalf("FetchWithKey: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("got %+v, want one model id=m1", got)
	}
	if seenAuth != "Bearer inline-key-123" {
		t.Fatalf("Authorization = %q, want the inline key as a bearer token", seenAuth)
	}
}

func TestFetchWithKey_EmptyKeyOmitsAuthHeader(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadAuth = r.Header["Authorization"]
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()

	if _, err := FetchWithKey(context.Background(), srv.URL, nil); err != nil {
		t.Fatalf("FetchWithKey: %v", err)
	}
	if hadAuth {
		t.Fatal("empty key should send NO Authorization header (keyless reachability probe)")
	}
}

func TestFetchWithKey_HTTPStatusError_CarriesStatusCode(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			fmt.Fprint(w, "boom")
		}))

		_, err := FetchWithKey(context.Background(), srv.URL, []byte("k"))
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: want error, got nil", code)
		}
		var hse *HTTPStatusError
		if !errors.As(err, &hse) {
			t.Fatalf("status %d: err = %v, want *HTTPStatusError", code, err)
		}
		if hse.StatusCode != code {
			t.Fatalf("HTTPStatusError.StatusCode = %d, want %d", hse.StatusCode, code)
		}
	}
}

func TestFetchWithKey_HTTP401_ReturnsErrKeyInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := FetchWithKey(context.Background(), srv.URL, []byte("k"))
	if !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("err = %v, want wrapped ErrKeyInvalid (not *HTTPStatusError)", err)
	}
	var hse *HTTPStatusError
	if errors.As(err, &hse) {
		t.Fatal("401 must map to the ErrKeyInvalid sentinel, not *HTTPStatusError")
	}
}

func TestFetch_SkipsEntriesWithEmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[
            {"id":"good","owned_by":"x"},
            {"id":"","owned_by":"x"},
            {"id":"good2"}
        ]}`)
	}))
	defer srv.Close()

	v := installVendor(t, "deepseek", srv.URL)

	got, err := Fetch(context.Background(), v)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 || got[0].ID != "good" || got[1].ID != "good2" {
		t.Fatalf("got %+v, want only the two non-empty ids", got)
	}
}
