package main

import (
	"github.com/ethanhq/cc-fleet/internal/config"
)

// providerErrorCode is config.ProviderErrorCode — the stable error_code a lane's
// JSON envelope exposes (the skill dispatches on it).
func providerErrorCode(err error) string { return config.ProviderErrorCode(err) }

// resolveProviderArg resolves a lane's provider: the requested name when given,
// else the configured / sole-enabled default. An empty requested with no
// resolvable default returns a sentinel (classify with providerErrorCode). An
// explicit name is returned verbatim — its existence/enabled checks stay on the
// lane's normal launch path, unchanged.
func resolveProviderArg(requested string) (string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	name, _, err := cfg.ResolveProvider(requested)
	return name, err
}

// firstArg returns args[0] or "" — the optional provider positional now that the
// lanes accept a default.
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
