package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// shrinkModelsBody temporarily shrinks the response-body cap for a test so the
// overflow path is exercised without streaming megabytes.
func shrinkModelsBody(t *testing.T, n int) {
	t.Helper()
	orig := maxModelsBody
	t.Cleanup(func() { maxModelsBody = orig })
	maxModelsBody = n
}

// TestFetchWithKey_OversizedBody_Rejected: a 2xx body over the cap is a hard,
// key-safe error (size only, no body bytes) — not a silent truncation that would
// mis-parse, and not an unbounded read.
func TestFetchWithKey_OversizedBody_Rejected(t *testing.T) {
	shrinkModelsBody(t, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxModelsBody+1)))
	}))
	defer srv.Close()

	_, err := FetchWithKey(context.Background(), srv.URL, []byte("k"))
	if err == nil {
		t.Fatal("oversized 200 body: want error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want an 'exceeds' size error", err)
	}
	// Key-safety: the error names a size, never echoes body bytes.
	if strings.Contains(err.Error(), "aaaa") {
		t.Fatalf("error leaked body bytes: %v", err)
	}
}

// TestFetchWithKey_Oversized401_StillKeyInvalid: bounding the read must NOT break
// status classification — an oversized 401 stays ErrKeyInvalid, not too-large.
func TestFetchWithKey_Oversized401_StillKeyInvalid(t *testing.T) {
	shrinkModelsBody(t, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(strings.Repeat("a", maxModelsBody+1)))
	}))
	defer srv.Close()

	_, err := FetchWithKey(context.Background(), srv.URL, []byte("k"))
	if !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("err = %v, want ErrKeyInvalid (classification must survive the cap)", err)
	}
}

// TestFetchWithKey_Oversized500_StillHTTPStatusError: an oversized 5xx stays a
// *HTTPStatusError carrying the status — not reclassified as too-large.
func TestFetchWithKey_Oversized500_StillHTTPStatusError(t *testing.T) {
	shrinkModelsBody(t, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("a", maxModelsBody+1)))
	}))
	defer srv.Close()

	_, err := FetchWithKey(context.Background(), srv.URL, []byte("k"))
	var hse *HTTPStatusError
	if !errors.As(err, &hse) || hse.StatusCode != http.StatusInternalServerError {
		t.Fatalf("err = %v, want *HTTPStatusError(500)", err)
	}
}
