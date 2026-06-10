// Package neterr centralizes the single question three call sites need to
// answer the same way: did a request fail at the transport layer — before any
// HTTP response came back?
//
// The provider probe, Add's synchronous probe, and `cc-fleet refresh` all classify
// a models-endpoint failure as either "provider unreachable" (network/DNS/dial/TLS
// /timeout) or "provider answered, just unhappily" (any HTTP status). This package
// is the one shared definition of that distinction.
package neterr

import (
	"context"
	"errors"
	"net"
	"net/url"
)

// IsTransport reports whether err is a connection-layer failure — i.e. no HTTP
// response was ever received. It is the union of the transport-layer cases the
// call sites care about:
//
//   - context.DeadlineExceeded — our own timeout fired
//   - *url.Error whose Timeout() is true — client-side request timeout
//   - *net.DNSError — name resolution failed
//   - *net.OpError — dial / connect / TLS / read failure
//
// Any other error (including an *url.Error that did not time out, e.g. a bad
// redirect) is treated as non-transport: the provider is reachable and the
// failure is something the caller should surface differently. Detection is
// structural (errors.Is / errors.As) so it survives wrapping, and IsTransport
// reports false for a nil error.
func IsTransport(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}
