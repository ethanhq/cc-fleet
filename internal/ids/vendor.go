package ids

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidVendorName is returned by ValidateVendorName for any input that
// fails the vendor-name grammar. Use errors.Is for dispatch.
var ErrInvalidVendorName = errors.New("invalid vendor name")

// vendorNameRe restricts vendor names to a letter prefix + alnum/_/- (max 32).
// The closed grammar kills shell-meta and path-separator names, which would
// otherwise reach a filepath.Join (profile path) and a shell-evaluated
// apiKeyHelper. The grammar lives in internal/ids (no cc-fleet imports) so
// internal/config can reject a malicious table name at Load time without an
// import cycle through userops.
var vendorNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)

// VendorNamePattern is the grammar as a string, exported so callers can quote
// the same pattern in error messages without re-stating the literal.
const VendorNamePattern = `^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`

// ValidateVendorName returns nil if name is a syntactically acceptable vendor
// identifier, or an error wrapping ErrInvalidVendorName describing the first
// rule it violated. Because a vendor name flows into a filesystem path
// (profiles/<name>.json) AND a shell-evaluated apiKeyHelper command, this is
// both a path-safety and a shell-injection guard.
func ValidateVendorName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidVendorName)
	}
	if !vendorNameRe.MatchString(name) {
		return fmt.Errorf("%w %q (must match %s — letter prefix, alnum/_/- only, max 32 chars)",
			ErrInvalidVendorName, name, VendorNamePattern)
	}
	return nil
}
