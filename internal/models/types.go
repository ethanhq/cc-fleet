// Package models caches and refreshes per-vendor model lists fetched from
// each vendor's `/v1/models` endpoint.
//
// The skill consults the cache to resolve `--model` choices before calling
// `cc-fleet spawn`; `cc-fleet refresh <vendor>` re-queries the vendor's HTTP
// endpoint to repopulate the cache.
//
// Nothing in this package logs vendor API keys; see fetch.go for the
// Authorization-header handling rules.
package models

import "time"

// Model is one entry returned by a vendor's /v1/models response.
//
// Field tags are part of the on-disk cache schema — do not rename without
// bumping Cache.Version.
type Model struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// VendorCache is one vendor's slot inside models-cache.json. The Endpoint
// is recorded alongside FetchedAt so callers can detect endpoint drift
// (user edited models_endpoint in vendors.toml after the last refresh).
type VendorCache struct {
	Vendor    string    `json:"vendor"`
	Endpoint  string    `json:"endpoint"`
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

// Cache is the full models-cache.json document.
//
// Vendors is keyed by vendor name (matches the table name in vendors.toml).
type Cache struct {
	Version int                     `json:"version"`
	Vendors map[string]*VendorCache `json:"vendors"`
}
