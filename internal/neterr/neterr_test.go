package neterr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"
)

// timeoutErr is a minimal error whose Timeout() reports true. It stands in for
// a client-side request timeout surfaced through *url.Error, without being
// context.DeadlineExceeded (which the first branch would catch on its own) — so
// it exercises the url.Error.Timeout() branch specifically.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

func TestIsTransport(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"deadline exceeded wrapped", fmt.Errorf("probe: %w", context.DeadlineExceeded), true},
		{"url timeout", &url.Error{Op: "Get", URL: "https://x.example/v1/models", Err: timeoutErr{}}, true},
		{"url timeout wrapped", fmt.Errorf("fetch: %w", &url.Error{Op: "Get", URL: "https://x.example", Err: timeoutErr{}}), true},
		{"dns error", &net.DNSError{Err: "no such host", Name: "x.example", IsNotFound: true}, true},
		{"dns error wrapped", fmt.Errorf("lookup: %w", &net.DNSError{Err: "no such host", Name: "x.example"}), true},
		{"op error dial refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"op error wrapped", fmt.Errorf("connect: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}), true},
		{"plain error", errors.New("vendor returned HTTP 500"), false},
		// An *url.Error that did NOT time out (e.g. a redirect failure) is the
		// vendor being reachable-but-unhappy, not a transport failure.
		{"url non-timeout", &url.Error{Op: "Get", URL: "https://x.example", Err: errors.New("stopped after 10 redirects")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransport(tc.err); got != tc.want {
				t.Fatalf("IsTransport(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
