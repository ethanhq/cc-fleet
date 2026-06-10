// Package models caches and refreshes per-provider model lists fetched from
// each provider's `/v1/models` endpoint.
//
// The skill consults the cache to resolve `--model` choices before calling
// `cc-fleet spawn`; `cc-fleet refresh <provider>` re-queries the provider's HTTP
// endpoint to repopulate the cache.
//
// Nothing in this package logs provider API keys; see fetch.go for the
// Authorization-header handling rules.
package models

import "time"

// Model is one entry returned by a provider's /v1/models response.
//
// Field tags are part of the on-disk cache schema — do not rename without
// bumping Cache.Version.
type Model struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ProviderCache is one provider's slot inside models-cache.json. The Endpoint
// is recorded alongside FetchedAt so callers can detect endpoint drift
// (user edited models_endpoint in providers.toml after the last refresh).
type ProviderCache struct {
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint"`
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

// Cache is the full models-cache.json document.
//
// Providers is keyed by provider name (matches the table name in providers.toml).
type Cache struct {
	Version   int                       `json:"version"`
	Providers map[string]*ProviderCache `json:"providers"`
}
