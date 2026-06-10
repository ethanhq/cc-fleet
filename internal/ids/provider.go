package ids

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidProviderName is returned by ValidateProviderName for any input that
// fails the provider-name grammar. Use errors.Is for dispatch.
var ErrInvalidProviderName = errors.New("invalid provider name")

// providerNameRe restricts provider names to a letter prefix + alnum/_/- (max 32).
// The closed grammar kills shell-meta and path-separator names, which would
// otherwise reach a filepath.Join (profile path) and a shell-evaluated
// apiKeyHelper. The grammar lives in internal/ids (no cc-fleet imports) so
// internal/config can reject a malicious table name at Load time without an
// import cycle through userops.
var providerNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`)

// ProviderNamePattern is the grammar as a string, exported so callers can quote
// the same pattern in error messages without re-stating the literal.
const ProviderNamePattern = `^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`

// ValidateProviderName returns nil if name is a syntactically acceptable provider
// identifier, or an error wrapping ErrInvalidProviderName describing the first
// rule it violated. Because a provider name flows into a filesystem path
// (profiles/<name>.json) AND a shell-evaluated apiKeyHelper command, this is
// both a path-safety and a shell-injection guard.
func ValidateProviderName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidProviderName)
	}
	if !providerNameRe.MatchString(name) {
		return fmt.Errorf("%w %q (must match %s — letter prefix, alnum/_/- only, max 32 chars)",
			ErrInvalidProviderName, name, ProviderNamePattern)
	}
	return nil
}
