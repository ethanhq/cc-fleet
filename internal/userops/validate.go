// Package userops implements the user-layer CRUD operations behind
// `cc-fleet init / add / edit / remove / list / repair / uninstall`. The cmd/
// files are thin wrappers around the Init/Add/Edit/Remove/List/Repair/Uninstall
// functions defined here, so the logic is unit-testable without cobra.
//
// Nothing in this package logs API key bytes. The only path that touches the
// raw key material is Add's file-backend write, which copies caller-supplied
// bytes into <SecretsDir>/<SecretRef> at mode 0600 and never echoes them.
package userops

import (
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// ValidateProviderName returns nil if name is a syntactically acceptable provider
// identifier, or an error describing the first rule it violated. Callers
// (Add, Edit, Remove) should run this on user input before touching the
// filesystem.
//
// The grammar itself lives in internal/ids (ids.ValidateProviderName) so
// internal/config — which cannot import userops (cycle) — can reject a
// hand-edited malicious providers.toml table name at Load time. This wrapper
// preserves the userops public API; the rule set is single-letter prefix,
// alnum/_/-, max 32 chars.
func ValidateProviderName(name string) error {
	return ids.ValidateProviderName(name)
}
