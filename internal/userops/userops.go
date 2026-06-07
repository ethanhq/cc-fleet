package userops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/neterr"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teamhist"
)

// Error codes returned by Add/Edit/Remove. Stable so cmd/ JSON envelopes can
// expose them to the skill without prose parsing.
const (
	CodeVendorNameInvalid  = "VENDOR_NAME_INVALID"
	CodeVendorExists       = "VENDOR_EXISTS"
	CodeVendorUnknown      = "VENDOR_UNKNOWN"
	CodeConfigLoadFailed   = "CONFIG_LOAD_FAILED"
	CodeConfigSaveFailed   = "CONFIG_SAVE_FAILED"
	CodeSecretWriteFailed  = "SECRET_WRITE_FAILED"
	CodeSecretRemoveFailed = "SECRET_REMOVE_FAILED"
	CodeProfileWriteFailed = "PROFILE_WRITE_FAILED"
	CodeKeyInvalid         = "KEY_INVALID"
	CodeVendorUnreachable  = "VENDOR_UNREACHABLE"
	CodeAddFailed          = "ADD_FAILED"
	CodeInvalidBackend     = "INVALID_BACKEND"
	CodeBackendUnsupported = "BACKEND_UNSUPPORTED"
	CodeInitFailed         = "INIT_FAILED"
	CodeRepairFailed       = "REPAIR_FAILED"
	CodeUninstallFailed    = "UNINSTALL_FAILED"
)

// probeTimeout caps the synchronous /v1/models probe Add performs after
// staging the vendor on disk. Matches doctor check 6's per-vendor budget.
const probeTimeout = 3 * time.Second

// Op is the typed-error returned by Add/Edit/Remove etc. cmd/ envelopes surface
// its Code as `error_code`.
type Op struct {
	Code string
	Err  error
}

func (e *Op) Error() string {
	if e == nil {
		return "<nil userops.Op>"
	}
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

func (e *Op) Unwrap() error { return e.Err }

func opErr(code string, err error) error { return &Op{Code: code, Err: err} }

// withVendorsLock runs fn under the global vendors.toml flock
// (config.WithVendorsConfigLock) so the load→mutate→save cycle of Add/Edit/
// Remove is serialized across concurrent cc-fleet processes — no lost updates.
// fn's own typed *Op errors propagate unchanged; only a raw lock-ACQUISITION
// failure (can't open the lock file) is normalized into a typed
// CONFIG_LOAD_FAILED Op so callers always see a stable error code.
func withVendorsLock[T any](fn func() (T, error)) (T, error) {
	var res T
	err := config.WithVendorsConfigLock(func() error {
		var e error
		res, e = fn()
		return e
	})
	if err != nil {
		var op *Op
		if errors.As(err, &op) {
			return res, err // fn's typed error — already carries its code
		}
		return res, opErr(CodeConfigLoadFailed, fmt.Errorf("acquire vendors config lock: %w", err))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// init
// ---------------------------------------------------------------------------

// InitResult is the structured result of Init.
type InitResult struct {
	Created    []string `json:"created"`
	AlreadyHad []string `json:"already_had"`
}

// Init creates the cc-fleet config directory tree and the empty vendors.toml if
// not yet present. The skill directory is created (so the install path is ready)
// but its contents are left to the install-skill step — Init does NOT copy
// SKILL.md.
//
// Idempotent: a fresh run on an already-initialized HOME is a no-op (returns
// Created=nil, AlreadyHad=[<paths>]). Returns an *Op on hard failures.
func Init() (*InitResult, error) {
	res := &InitResult{}

	cfgDir, err := config.ConfigDir()
	if err != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("resolve config dir: %w", err))
	}
	secretsDir, err := config.SecretsDir()
	if err != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("resolve secrets dir: %w", err))
	}
	profilesDir, err := profile.ProfilesDir()
	if err != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("resolve profiles dir: %w", err))
	}

	home := os.Getenv("HOME")
	if home == "" {
		return nil, opErr(CodeInitFailed, errors.New("HOME is not set"))
	}
	skillDir := filepath.Join(home, ".claude", "skills", "cc-fleet")

	dirs := []string{cfgDir, secretsDir, profilesDir, skillDir}
	for _, d := range dirs {
		if existed, mkErr := ensureDir(d, 0o700); mkErr != nil {
			return nil, opErr(CodeInitFailed, fmt.Errorf("mkdir %s: %w", d, mkErr))
		} else if existed {
			res.AlreadyHad = append(res.AlreadyHad, d)
		} else {
			res.Created = append(res.Created, d)
		}
	}

	// Empty vendors.toml on first run so subsequent Load/Save calls have a
	// stable schema-version line. We don't overwrite an existing file (would
	// destroy user vendors), but we do create a fresh one if missing.
	vendorsPath, err := config.VendorsPath()
	if err != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("resolve vendors path: %w", err))
	}
	if _, statErr := os.Stat(vendorsPath); errors.Is(statErr, os.ErrNotExist) {
		cfg := &config.Config{
			Version: config.SchemaVersion,
			Vendors: map[string]*config.Vendor{},
		}
		if err := config.SaveToPath(cfg, vendorsPath); err != nil {
			return nil, opErr(CodeInitFailed, fmt.Errorf("write empty vendors.toml: %w", err))
		}
		res.Created = append(res.Created, vendorsPath)
	} else if statErr != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("stat vendors.toml: %w", statErr))
	} else {
		res.AlreadyHad = append(res.AlreadyHad, vendorsPath)
	}

	return res, nil
}

// ensureDir mkdir's path at mode and reports whether it already existed.
// Returns (true, nil) if path was already a directory, (false, nil) if it
// was created, and (_, err) on hard failure (parent missing, perm denied, etc).
func ensureDir(path string, mode os.FileMode) (alreadyExisted bool, err error) {
	info, statErr := os.Stat(path)
	if statErr == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("%s exists but is not a directory", path)
		}
		return true, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return false, err
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// add
// ---------------------------------------------------------------------------

// AddRequest is the typed input for Add.
//
// APIKey is optional; when set, it MUST be paired with SecretBackend="file"
// and SecretRef!="". Other backends reject inline keys (use the backend's
// native tool — `pass insert`, `op item edit`, etc.).
type AddRequest struct {
	Name           string
	BaseURL        string
	ModelsEndpoint string
	DefaultModel   string
	SecretBackend  string
	SecretRef      string
	APIKey         string // raw key bytes; only valid with SecretBackend=="file"
	Enabled        bool   // defaults to true at the caller layer
}

// AddResult is the structured success result of Add.
type AddResult struct {
	Vendor      string    `json:"vendor"`
	ProfilePath string    `json:"profile_path"`
	AddedAt     time.Time `json:"added_at"`
	ModelCount  int       `json:"model_count"`
}

// Add stages a new vendor end-to-end:
//
//  1. validate the requested vendor name (regex)
//  2. refuse to overwrite an existing entry (use Edit/Remove instead)
//  3. if APIKey is set, write it to <SecretsDir>/<SecretRef> at 0600
//  4. probe the vendor's /v1/models endpoint (3s) using the same secrets path
//     keyget would — failures map to KEY_INVALID, VENDOR_UNREACHABLE, ADD_FAILED
//  5. commit vendors.toml + write profile JSON + populate the models cache
//
// On any failure after step 3, the file-backend secret is rolled back so the
// caller can re-run with a corrected key.
//
// The whole load→mutate→save cycle runs under the global vendors-config flock
// so concurrent add/edit/remove can't lose each other's updates.
func Add(req AddRequest) (*AddResult, error) {
	return withVendorsLock(func() (*AddResult, error) { return addLocked(req) })
}

func addLocked(req AddRequest) (*AddResult, error) {
	if err := ValidateVendorName(req.Name); err != nil {
		return nil, opErr(CodeVendorNameInvalid, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	if _, exists := cfg.Vendors[req.Name]; exists {
		return nil, opErr(CodeVendorExists,
			fmt.Errorf("vendor %q already exists; use `cc-fleet edit` or `cc-fleet remove` first", req.Name))
	}

	// Inline-key handling: only file backend supports writing the key for the
	// user. Other backends require the user to provision the secret through
	// the backend's own CLI.
	if req.APIKey != "" {
		if req.SecretBackend != "file" {
			return nil, opErr(CodeBackendUnsupported,
				fmt.Errorf("--api-key is only supported with --secret-backend file (got %q)", req.SecretBackend))
		}
		if req.SecretRef == "" {
			return nil, opErr(CodeInvalidBackend, errors.New("--secret-ref is required when --api-key is set"))
		}
		if err := secrets.SafeRef(req.SecretRef); err != nil {
			return nil, opErr(CodeInvalidBackend, err)
		}
		if err := writeFileSecret(req.SecretRef, []byte(req.APIKey)); err != nil {
			return nil, opErr(CodeSecretWriteFailed, err)
		}
	}

	// Build the candidate vendor and validate schema before any further work.
	v := &config.Vendor{
		Name:           req.Name,
		BaseURL:        req.BaseURL,
		ModelsEndpoint: req.ModelsEndpoint,
		DefaultModel:   req.DefaultModel,
		SecretBackend:  req.SecretBackend,
		SecretRef:      req.SecretRef,
		Enabled:        req.Enabled,
		AddedAt:        time.Now().UTC(),
	}

	// Merge the new vendor into the loaded config and validate before any
	// disk write. We persist the full merged config so existing vendors are
	// preserved on success and easy to restore on probe failure (we just
	// re-delete the staged vendor and re-save).
	cfg.Vendors[req.Name] = v
	if err := cfg.Validate(); err != nil {
		delete(cfg.Vendors, req.Name)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeAddFailed, err)
	}

	// The probe re-uses the normal secrets.Keyget path, which loads
	// vendors.toml internally. Persist the staged config so Keyget can
	// find it, then roll back vendors.toml + secret if the probe fails.
	if err := config.Save(cfg); err != nil {
		delete(cfg.Vendors, req.Name)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeConfigSaveFailed, err)
	}

	// Synchronous probe so a bad key fails the add (not silently later). Failure
	// here rolls back vendors.toml + the inline secret.
	probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	fetched, fetchErr := models.Fetch(probeCtx, v)
	if fetchErr != nil {
		// Roll back the staged vendor row before returning the typed error.
		delete(cfg.Vendors, req.Name)
		_ = config.Save(cfg)
		_ = rollbackInlineSecret(req)
		return nil, opErr(classifyAddErr(fetchErr), fetchErr)
	}

	// All good — write the profile JSON and refresh the models cache so the
	// user can immediately `cc-fleet models <vendor>` without an explicit
	// `cc-fleet refresh`.
	path, err := profile.WriteForVendor(v, "")
	if err != nil {
		delete(cfg.Vendors, req.Name)
		_ = config.Save(cfg)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeProfileWriteFailed, err)
	}

	if err := updateModelsCache(req.Name, v.ModelsEndpoint, fetched); err != nil {
		// Cache write failure is non-fatal: the vendor is fully staged and the
		// cache repopulates on the next `cc-fleet refresh`. Warn on stderr (so a
		// --json caller's stdout envelope stays clean) but don't fail the add.
		// The error is a cache/file error, never a key.
		fmt.Fprintf(os.Stderr, "warning: vendor %s added but models cache update failed: %v\n", req.Name, err)
	}

	return &AddResult{
		Vendor:      req.Name,
		ProfilePath: path,
		AddedAt:     v.AddedAt,
		ModelCount:  len(fetched),
	}, nil
}

// classifyAddErr maps a models.Fetch failure during Add onto an error code.
// KEY_INVALID is the sentinel and beats transport classification.
func classifyAddErr(err error) string {
	if errors.Is(err, models.ErrKeyInvalid) {
		return CodeKeyInvalid
	}
	if neterr.IsTransport(err) {
		return CodeVendorUnreachable
	}
	return CodeAddFailed
}

// writeFileSecret atomically writes data to <SecretsDir>/<ref> at mode 0600.
// The parent directory is created at 0700 if missing. Trailing CR/LF on data is
// kept as supplied — secrets.Keyget trims them on read.
//
// The write stages into a same-directory temp file then renames over the target,
// so a failed/partial write can never truncate an existing key (matters most for
// Edit's in-place key rotation: the previous key stays usable rather than
// clobbered). The temp file is removed on any failure.
func writeFileSecret(ref string, data []byte) error {
	// Choke point: ref builds a path under SecretsDir, so reject any value that
	// could escape it (Add/Edit validate earlier for a cleaner error code; this
	// guards every other caller).
	if err := secrets.SafeRef(ref); err != nil {
		return err
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return fmt.Errorf("resolve secrets dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, ref)

	if err := fileutil.AtomicWrite(path, data, 0o600); err != nil {
		return fmt.Errorf("write secret %s: %w", path, err)
	}
	return nil
}

// rollbackInlineSecret removes the file-backend secret that Add wrote when
// the rest of the add flow fails. Caller-provisioned secrets (no APIKey in
// the request) are never touched. A missing file is fine — best-effort.
//
// Routed through removeFileSecret so the SafeRef choke-point guard applies here
// too (funneling every SecretsDir delete through one guarded helper keeps a
// future caller from regressing it).
func rollbackInlineSecret(req AddRequest) error {
	if req.APIKey == "" || req.SecretBackend != "file" || req.SecretRef == "" {
		return nil
	}
	return removeFileSecret(req.SecretRef)
}

// updateModelsCache writes the fresh probe result into models-cache.json so
// the user doesn't need an explicit `cc-fleet refresh` right after `add`.
// Failure here is non-fatal at the Add layer — the cache will repopulate on
// the next refresh.
func updateModelsCache(vendor, endpoint string, fetched []models.Model) error {
	cache, err := models.Load()
	if err != nil {
		return fmt.Errorf("load cache: %w", err)
	}
	if cache.Vendors == nil {
		cache.Vendors = map[string]*models.VendorCache{}
	}
	cache.Vendors[vendor] = &models.VendorCache{
		Vendor:    vendor,
		Endpoint:  endpoint,
		FetchedAt: time.Now().UTC(),
		Models:    fetched,
	}
	return models.Save(cache)
}

// ---------------------------------------------------------------------------
// edit
// ---------------------------------------------------------------------------

// EditRequest is the typed input for Edit. Only non-nil pointer fields apply.
// Pointer types let callers distinguish "leave unchanged" from "set to zero".
type EditRequest struct {
	Name           string  // required
	BaseURL        *string // nil = no change
	ModelsEndpoint *string
	DefaultModel   *string
	SecretBackend  *string
	SecretRef      *string
	Enabled        *bool
	// KeyRotation sets the file-backend multi-key rotation strategy
	// ("off"|"round_robin"|"random"). nil leaves it unchanged; the merged value
	// is checked by config.Validate.
	KeyRotation *string
	// APIKey rotates the file-backend secret in place. Unlike the fields above
	// it is a plain string ("" = no change) — there is no "set the key to
	// empty" use case, so the cmd layer's non-empty gate is sufficient. Mirrors
	// AddRequest.APIKey; only valid when the vendor's effective
	// SecretBackend=="file" (other backends manage their own secrets).
	APIKey string
}

// EditResult mirrors the post-edit vendor row (skill consumers parse this to
// surface the new values to the user without re-running list).
type EditResult struct {
	Vendor *config.Vendor `json:"vendor"`
}

// Edit mutates the named vendor in place. Only fields set in req are applied;
// the rest are preserved. A schema-violating edit is rolled back before any
// disk write. base_url changes also re-render the profile JSON so Claude
// Code picks up the new ANTHROPIC_BASE_URL on the next spawn.
//
// Runs under the global vendors-config flock.
func Edit(req EditRequest) (*EditResult, error) {
	return withVendorsLock(func() (*EditResult, error) { return editLocked(req) })
}

func editLocked(req EditRequest) (*EditResult, error) {
	if err := ValidateVendorName(req.Name); err != nil {
		return nil, opErr(CodeVendorNameInvalid, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	v, ok := cfg.Vendors[req.Name]
	if !ok {
		return nil, opErr(CodeVendorUnknown,
			fmt.Errorf("vendor %q not in vendors.toml", req.Name))
	}

	baseURLChanged := false
	if req.BaseURL != nil && *req.BaseURL != v.BaseURL {
		v.BaseURL = *req.BaseURL
		baseURLChanged = true
	}
	if req.ModelsEndpoint != nil {
		v.ModelsEndpoint = *req.ModelsEndpoint
	}
	if req.DefaultModel != nil {
		v.DefaultModel = *req.DefaultModel
	}
	if req.SecretBackend != nil {
		v.SecretBackend = *req.SecretBackend
	}
	if req.SecretRef != nil {
		v.SecretRef = *req.SecretRef
	}
	if req.Enabled != nil {
		v.Enabled = *req.Enabled
	}
	if req.KeyRotation != nil {
		v.KeyRotation = *req.KeyRotation // config.Validate rejects bad values
	}

	// Inline key rotation (file backend only), mirroring Add. We validate
	// against the EFFECTIVE backend/ref — i.e. after the merge above — so the
	// user can rotate a key using the vendor's existing secret_ref without
	// re-passing --secret-ref. Non-file backends provision keys themselves.
	if req.APIKey != "" {
		if v.SecretBackend != "file" {
			return nil, opErr(CodeBackendUnsupported,
				fmt.Errorf("--api-key is only supported with secret backend file (vendor %q uses %q)", req.Name, v.SecretBackend))
		}
		// Multi-key guard: once a vendor has a keys.json, the inline single-key
		// path would silently write to the now-ignored legacy file. Refuse and
		// point at the TUI (the multi-key human entry point). The message carries
		// no key bytes.
		multi, mErr := secrets.IsMultiKey(req.Name)
		if mErr != nil {
			return nil, opErr(CodeConfigLoadFailed, fmt.Errorf("check multi-key state: %w", mErr))
		}
		if multi {
			return nil, opErr(CodeBackendUnsupported,
				fmt.Errorf("vendor %q has multiple keys; manage them in the TUI (cc-fleet)", req.Name))
		}
		if v.SecretRef == "" {
			return nil, opErr(CodeInvalidBackend,
				errors.New("--api-key needs a secret_ref; pass --secret-ref to set one"))
		}
		if err := secrets.SafeRef(v.SecretRef); err != nil {
			return nil, opErr(CodeInvalidBackend, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, opErr(CodeAddFailed, err)
	}

	// Write the rotated key BEFORE saving vendors.toml: key-first means a later
	// Save failure leaves the config still pointing at the OLD (intact) ref so
	// the vendor keeps working; save-first would point at a new ref whose key
	// doesn't exist yet, breaking the vendor if the write then failed.
	if req.APIKey != "" {
		if err := writeFileSecret(v.SecretRef, []byte(req.APIKey)); err != nil {
			return nil, opErr(CodeSecretWriteFailed, err)
		}
	}

	if err := config.Save(cfg); err != nil {
		return nil, opErr(CodeConfigSaveFailed, err)
	}

	// Re-render the profile only when base_url moved — the apiKeyHelper path is
	// identical between vendors, so other field edits don't affect it.
	if baseURLChanged {
		if _, err := profile.WriteForVendor(v, ""); err != nil {
			return nil, opErr(CodeProfileWriteFailed, err)
		}
	}

	return &EditResult{Vendor: v}, nil
}

// ---------------------------------------------------------------------------
// remove
// ---------------------------------------------------------------------------

// RemoveRequest is the typed input for Remove.
type RemoveRequest struct {
	Name       string
	KeepSecret bool // when true, file-backend secrets are preserved
}

// RemoveResult is the structured result of Remove.
type RemoveResult struct {
	Vendor         string `json:"removed"`
	SecretRemoved  bool   `json:"secret_removed"`
	ProfileRemoved bool   `json:"profile_removed"`
}

// Remove deletes the vendor row from vendors.toml, the per-vendor profile
// JSON, and (for file-backend vendors without --keep-secret) the on-disk
// secret. Non-file backends are never auto-purged — the user removes those
// through the backend's own CLI.
//
// Runs under the global vendors-config flock.
func Remove(req RemoveRequest) (*RemoveResult, error) {
	return withVendorsLock(func() (*RemoveResult, error) { return removeLocked(req) })
}

func removeLocked(req RemoveRequest) (*RemoveResult, error) {
	if err := ValidateVendorName(req.Name); err != nil {
		return nil, opErr(CodeVendorNameInvalid, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	v, ok := cfg.Vendors[req.Name]
	if !ok {
		return nil, opErr(CodeVendorUnknown,
			fmt.Errorf("vendor %q not in vendors.toml", req.Name))
	}

	res := &RemoveResult{Vendor: req.Name}

	// Commit the config row deletion FIRST, before any destructive profile/secret
	// cleanup: if Save fails, vendors.toml is unchanged and the profile + secret
	// are still intact — never a config row pointing at already-deleted artifacts.
	// Only once the row is durably gone do we reap the now-unreferenced profile +
	// secret; a failure there leaves a harmless orphan, never a dangling
	// reference or destroyed key the config still claims exists. (v still points
	// at the removed Vendor struct — the map entry is gone but the value is held.)
	delete(cfg.Vendors, req.Name)
	if err := config.Save(cfg); err != nil {
		return nil, opErr(CodeConfigSaveFailed, err)
	}

	// Profile removal is idempotent (RemoveForVendor swallows ENOENT).
	if err := profile.RemoveForVendor(req.Name); err != nil {
		return nil, opErr(CodeProfileWriteFailed, err)
	}
	res.ProfileRemoved = true

	// Secret cleanup: only file backend is auto-purged, and only when
	// the user didn't ask to keep it.
	if v.SecretBackend == "file" && !req.KeepSecret {
		if v.SecretRef != "" {
			if err := removeFileSecret(v.SecretRef); err != nil {
				return nil, opErr(CodeSecretRemoveFailed, err)
			}
			res.SecretRemoved = true
		}
		// Also purge the multi-key store + rotation counter (best-effort; a
		// missing file is not an error). Independent of secret_ref because a
		// multi-key vendor's keys live in <vendor>.keys.json, not secret_ref.
		if err := secrets.RemoveKeySet(req.Name); err != nil {
			return nil, opErr(CodeSecretRemoveFailed, err)
		}
	}

	return res, nil
}

// removeFileSecret deletes <SecretsDir>/<ref>. A missing file is fine —
// idempotent cleanup is the point.
func removeFileSecret(ref string) error {
	// Same choke-point guard as writeFileSecret: never join an unsafe ref onto
	// SecretsDir, even for a delete.
	if err := secrets.SafeRef(ref); err != nil {
		return err
	}
	dir, err := config.SecretsDir()
	if err != nil {
		return fmt.Errorf("resolve secrets dir: %w", err)
	}
	path := filepath.Join(dir, ref)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

// VendorView is the per-vendor JSON shape `cc-fleet list --json` emits. Kept
// flat (no nested objects) so jq dispatch in the skill stays trivial.
type VendorView struct {
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	DefaultModel   string `json:"default_model"`
	ModelsEndpoint string `json:"models_endpoint"`
	SecretBackend  string `json:"secret_backend"`
	SecretRef      string `json:"secret_ref"`
	Enabled        bool   `json:"enabled"`
	ModelsCount    int    `json:"models_count"`
	ModelsStale    bool   `json:"models_stale"`
}

// ListResult is the structured result of List. Vendors is always non-nil even
// when empty so JSON consumers can iterate without a presence check.
type ListResult struct {
	Vendors []VendorView `json:"vendors"`
}

// List enumerates all configured vendors in alphabetical order. Each row is
// annotated with the local cache state (count + staleness) so callers can
// show "(stale)" without an extra round-trip.
func List() (*ListResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	cache, err := models.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, fmt.Errorf("load models cache: %w", err))
	}

	names := make([]string, 0, len(cfg.Vendors))
	for n := range cfg.Vendors {
		names = append(names, n)
	}
	sort.Strings(names)

	out := &ListResult{Vendors: []VendorView{}}
	for _, name := range names {
		v := cfg.Vendors[name]
		view := VendorView{
			Name:           v.Name,
			BaseURL:        v.BaseURL,
			DefaultModel:   v.DefaultModel,
			ModelsEndpoint: v.ModelsEndpoint,
			SecretBackend:  v.SecretBackend,
			SecretRef:      v.SecretRef,
			Enabled:        v.Enabled,
		}
		if vc, ok := cache.Vendors[name]; ok && vc != nil {
			view.ModelsCount = len(vc.Models)
			view.ModelsStale = models.IsStale(vc)
		} else {
			// No cache entry = stale by convention so the user knows a
			// refresh is needed.
			view.ModelsStale = true
		}
		out.Vendors = append(out.Vendors, view)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// repair
// ---------------------------------------------------------------------------

// RepairResult is the structured result of Repair.
type RepairResult struct {
	Repaired []string `json:"repaired"`
}

// Repair re-writes every vendor's profile JSON from the current vendors.toml.
// Secrets are NOT touched (Repair fixes profiles users may have accidentally
// deleted; secret backends own their own state).
func Repair() (*RepairResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	names := make([]string, 0, len(cfg.Vendors))
	for n := range cfg.Vendors {
		names = append(names, n)
	}
	sort.Strings(names)

	res := &RepairResult{Repaired: []string{}}
	for _, name := range names {
		v := cfg.Vendors[name]
		if _, err := profile.WriteForVendor(v, ""); err != nil {
			return nil, opErr(CodeRepairFailed,
				fmt.Errorf("rewrite profile for %q: %w", name, err))
		}
		res.Repaired = append(res.Repaired, name)
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// uninstall
// ---------------------------------------------------------------------------

// UninstallRequest is the typed input for Uninstall.
type UninstallRequest struct {
	KeepSecrets bool // default true at the caller layer
}

// UninstallResult is the structured result of Uninstall.
type UninstallResult struct {
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
}

// Uninstall removes every cc-fleet-owned file: per-vendor profile JSONs,
// vendors.toml, fingerprint.json, models-cache.json, the team-history records
// under teams-history/, and finished background
// jobs under subagent-jobs/ (see subagent.PurgeJobs — finished job files are
// removed even when other jobs are still running; the live ones, and the dir
// itself, are kept and reported in Kept). The skill directory
// (~/.claude/skills/cc-fleet/) and ~/.claude/teams/ are explicitly
// preserved — the former is owned by the install machinery, the latter is
// Claude Code's own state.
//
// Per-vendor file-backend secrets are removed unless KeepSecrets is true (the
// caller-level default). The whole <SecretsDir>/ tree is removed only when
// KeepSecrets is false.
func Uninstall(req UninstallRequest) (*UninstallResult, error) {
	res := &UninstallResult{
		Removed: []string{},
		Kept:    []string{},
	}

	cfg, err := config.Load()
	if err != nil {
		// Treat a malformed vendors.toml as "no vendors known" — Uninstall
		// should still get the rest of the tree clean. Surface in Kept so
		// the user can see what was skipped.
		res.Kept = append(res.Kept, fmt.Sprintf("vendors.toml (load failed: %v)", err))
		cfg = &config.Config{Version: config.SchemaVersion, Vendors: map[string]*config.Vendor{}}
	}

	// 1. Per-vendor profiles.
	for name := range cfg.Vendors {
		path, perr := profile.ProfilePath(name)
		if perr != nil {
			// Per-vendor failure shouldn't sink the whole uninstall.
			res.Kept = append(res.Kept, fmt.Sprintf("profile %s (resolve failed: %v)", name, perr))
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Kept = append(res.Kept, fmt.Sprintf("%s (remove failed: %v)", path, err))
			continue
		}
		res.Removed = append(res.Removed, path)
	}

	// 2. Top-level cc-fleet files inside ConfigDir.
	cfgDir, err := config.ConfigDir()
	if err != nil {
		return nil, opErr(CodeUninstallFailed, fmt.Errorf("resolve config dir: %w", err))
	}
	tops := []string{
		filepath.Join(cfgDir, "vendors.toml"),
		filepath.Join(cfgDir, "fingerprint.json"),
		filepath.Join(cfgDir, "models-cache.json"),
	}
	for _, p := range tops {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Kept = append(res.Kept, fmt.Sprintf("%s (remove failed: %v)", p, err))
			continue
		}
		res.Removed = append(res.Removed, p)
	}

	// 2b. Background subagent job files (ConfigDir/subagent-jobs). A job's
	// .out/.err/.result.json can hold prompt/answer fragments, so uninstall
	// should clean them too. Routed through subagent.PurgeJobs so the dir name +
	// per-job file group + running/finished judgement stay owned by the subagent
	// package (no hardcoded "subagent-jobs" path here). PurgeJobs does a PARTIAL
	// clean: it removes every FINISHED job's files even when some jobs are still
	// running, keeping only the live ones — so finished (possibly sensitive)
	// artifacts are cleaned while a live background subagent's files are never
	// yanked out from under it. KeepSecrets does not apply (job artifacts aren't
	// keys).
	jobsPath, removedFinished, running, perr := subagent.PurgeJobs()
	switch {
	case perr != nil:
		res.Kept = append(res.Kept, fmt.Sprintf("%s (purge failed: %v)", jobsPath, perr))
	case len(running) > 0:
		// Some jobs still running: finished jobs WERE removed; the live ones kept.
		// Report both — removed-finished in Removed (only when any were), the live
		// ones in Kept — plus a stderr note (keeps a --json caller's stdout clean).
		if len(removedFinished) > 0 {
			res.Removed = append(res.Removed,
				fmt.Sprintf("%s (%d finished job(s))", jobsPath, len(removedFinished)))
		}
		fmt.Fprintf(os.Stderr,
			"warning: removed %d finished subagent job(s); kept %d still running in %s. "+
				"Re-run `cc-fleet uninstall` (or `cc-fleet subagent-gc --older-than 1s`) "+
				"after they finish to remove them: %s\n",
			len(removedFinished), len(running), jobsPath, strings.Join(running, ", "))
		res.Kept = append(res.Kept, fmt.Sprintf(
			"%s (%d running job(s): %s; re-run uninstall or `cc-fleet subagent-gc --older-than 1s` after they finish)",
			jobsPath, len(running), strings.Join(running, ", ")))
	default:
		// Nothing left running — the whole dir was purged (or never existed).
		res.Removed = append(res.Removed, jobsPath)
	}

	// 2c. Team-history records (ConfigDir/teams-history). A record holds an ended
	// team's snapshot (member names, models, cwds) the board renders after the team
	// is gone; uninstall purges the whole dir. Routed through teamhist.Purge so the
	// dir name stays owned by that package (no hardcoded path here).
	if histPath, herr := teamhist.Purge(); herr != nil {
		res.Kept = append(res.Kept, fmt.Sprintf("%s (purge failed: %v)", histPath, herr))
	} else {
		res.Removed = append(res.Removed, histPath)
	}

	// 3. Secrets directory (or its contents, depending on KeepSecrets).
	secretsDir, err := config.SecretsDir()
	if err != nil {
		return nil, opErr(CodeUninstallFailed, fmt.Errorf("resolve secrets dir: %w", err))
	}
	if req.KeepSecrets {
		res.Kept = append(res.Kept, secretsDir)
	} else {
		if err := os.RemoveAll(secretsDir); err != nil {
			res.Kept = append(res.Kept, fmt.Sprintf("%s (rm -rf failed: %v)", secretsDir, err))
		} else {
			res.Removed = append(res.Removed, secretsDir)
		}
	}

	// 4. Explicitly preserved paths — surface in Kept so users know we
	// considered them. Skill dir is the install machinery's; teams/ is Claude Code's.
	home := os.Getenv("HOME")
	if home != "" {
		skillDir := filepath.Join(home, ".claude", "skills", "cc-fleet")
		teamsDir := filepath.Join(home, ".claude", "teams")
		if _, err := os.Stat(skillDir); err == nil {
			res.Kept = append(res.Kept, skillDir)
		}
		if _, err := os.Stat(teamsDir); err == nil {
			res.Kept = append(res.Kept, teamsDir)
		}
	}

	return res, nil
}
