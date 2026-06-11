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

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/homedir"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/neterr"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/selfupdate"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teamhist"
)

// Error codes returned by Add/Edit/Remove. Stable so cmd/ JSON envelopes can
// expose them to the skill without prose parsing.
const (
	CodeProviderNameInvalid = "PROVIDER_NAME_INVALID"
	CodeProviderExists      = "PROVIDER_EXISTS"
	CodeProviderUnknown     = "PROVIDER_UNKNOWN"
	CodeConfigLoadFailed    = "CONFIG_LOAD_FAILED"
	CodeConfigSaveFailed    = "CONFIG_SAVE_FAILED"
	CodeSecretWriteFailed   = "SECRET_WRITE_FAILED"
	CodeSecretRemoveFailed  = "SECRET_REMOVE_FAILED"
	CodeProfileWriteFailed  = "PROFILE_WRITE_FAILED"
	CodeKeyInvalid          = "KEY_INVALID"
	CodeProviderUnreachable = "PROVIDER_UNREACHABLE"
	CodeAddFailed           = "ADD_FAILED"
	CodeInvalidBackend      = "INVALID_BACKEND"
	CodeBackendUnsupported  = "BACKEND_UNSUPPORTED"
	CodeInitFailed          = "INIT_FAILED"
	CodeRepairFailed        = "REPAIR_FAILED"
	CodeUninstallFailed     = "UNINSTALL_FAILED"
	CodeDefaultAlreadySet   = "DEFAULT_ALREADY_SET"
)

// probeTimeout caps the synchronous /v1/models probe Add performs after
// staging the provider on disk. Matches doctor check 6's per-provider budget.
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

// withProvidersLock runs fn under the global providers.toml flock
// (config.WithProvidersConfigLock) so the load→mutate→save cycle of Add/Edit/
// Remove is serialized across concurrent cc-fleet processes — no lost updates.
// fn's own typed *Op errors propagate unchanged; only a raw lock-ACQUISITION
// failure (can't open the lock file) is normalized into a typed
// CONFIG_LOAD_FAILED Op so callers always see a stable error code.
func withProvidersLock[T any](fn func() (T, error)) (T, error) {
	var res T
	err := config.WithProvidersConfigLock(func() error {
		var e error
		res, e = fn()
		return e
	})
	if err != nil {
		var op *Op
		if errors.As(err, &op) {
			return res, err // fn's typed error — already carries its code
		}
		return res, opErr(CodeConfigLoadFailed, fmt.Errorf("acquire providers config lock: %w", err))
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

// Init creates the cc-fleet config directory tree and the empty providers.toml if
// not yet present. The ~/.claude/skills ROOT is created (so an install path is
// ready) but its contents are left to the install-skill step — Init does NOT copy
// any SKILL.md.
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
	// The skills ROOT (not a specific skill dir) — the install machinery owns the
	// per-lane dirs (cc-fleet-subagent/team/workflow) + shared; Init just makes the
	// root exist so an install path is ready.
	skillsRoot := filepath.Join(home, ".claude", "skills")

	dirs := []string{cfgDir, secretsDir, profilesDir, skillsRoot}
	for _, d := range dirs {
		if existed, mkErr := ensureDir(d, 0o700); mkErr != nil {
			return nil, opErr(CodeInitFailed, fmt.Errorf("mkdir %s: %w", d, mkErr))
		} else if existed {
			res.AlreadyHad = append(res.AlreadyHad, d)
		} else {
			res.Created = append(res.Created, d)
		}
	}

	// Empty providers.toml on first run so subsequent Load/Save calls have a
	// stable schema-version line. We don't overwrite an existing file (would
	// destroy user providers), but we do create a fresh one if missing.
	providersPath, err := config.ProvidersPath()
	if err != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("resolve providers path: %w", err))
	}
	if _, statErr := os.Stat(providersPath); errors.Is(statErr, os.ErrNotExist) {
		cfg := &config.Config{
			Version:   config.SchemaVersion,
			Providers: map[string]*config.Provider{},
		}
		if err := config.SaveToPath(cfg, providersPath); err != nil {
			return nil, opErr(CodeInitFailed, fmt.Errorf("write empty providers.toml: %w", err))
		}
		res.Created = append(res.Created, providersPath)
	} else if statErr != nil {
		return nil, opErr(CodeInitFailed, fmt.Errorf("stat providers.toml: %w", statErr))
	} else {
		res.AlreadyHad = append(res.AlreadyHad, providersPath)
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
	StrongModel    string // optional "strong" capability slot; blank → follows DefaultModel
	FastModel      string // optional "fast" capability slot; blank → follows DefaultModel
	Effort         string // optional reasoning-effort level (config.validEfforts); "" = unset
	DefaultPerm    string // optional default permission mode for `cc-fleet run`; "" = unset
	SecretBackend  string
	SecretRef      string
	Protocol       string // "" Anthropic-native | openai-chat | openai-responses | codex-oauth
	UpstreamURL    string // real OpenAI base URL; required for openai-* protocols
	APIKey         string // raw key bytes; only valid with SecretBackend=="file"
	Enabled        bool   // defaults to true at the caller layer
}

// AddResult is the structured success result of Add.
type AddResult struct {
	Provider    string    `json:"provider"`
	ProfilePath string    `json:"profile_path"`
	AddedAt     time.Time `json:"added_at"`
	ModelCount  int       `json:"model_count"`
}

// Add stages a new provider end-to-end:
//
//  1. validate the requested provider name (regex)
//  2. refuse to overwrite an existing entry (use Edit/Remove instead)
//  3. if APIKey is set, write it to <SecretsDir>/<SecretRef> at 0600
//  4. probe the provider's /v1/models endpoint (3s) using the same secrets path
//     keyget would — failures map to KEY_INVALID, PROVIDER_UNREACHABLE, ADD_FAILED.
//     A codex-oauth provider skips the probe (its models endpoint is a lazily-started
//     loopback daemon) and seeds the static codex model list instead.
//  5. commit providers.toml + write profile JSON + populate the models cache
//
// On any failure after step 3, the file-backend secret is rolled back so the
// caller can re-run with a corrected key.
//
// The whole load→mutate→save cycle runs under the global providers-config flock
// so concurrent add/edit/remove can't lose each other's updates.
func Add(req AddRequest) (*AddResult, error) {
	return withProvidersLock(func() (*AddResult, error) { return addLocked(req) })
}

func addLocked(req AddRequest) (*AddResult, error) {
	if err := ValidateProviderName(req.Name); err != nil {
		return nil, opErr(CodeProviderNameInvalid, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	if _, exists := cfg.Providers[req.Name]; exists {
		return nil, opErr(CodeProviderExists,
			fmt.Errorf("provider %q already exists; use `cc-fleet edit` or `cc-fleet remove` first", req.Name))
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

	// Build the candidate provider and validate schema before any further work.
	v := &config.Provider{
		Name:              req.Name,
		BaseURL:           req.BaseURL,
		ModelsEndpoint:    req.ModelsEndpoint,
		DefaultModel:      req.DefaultModel,
		StrongModel:       req.StrongModel,
		FastModel:         req.FastModel,
		Effort:            req.Effort,
		DefaultPermission: req.DefaultPerm,
		SecretBackend:     req.SecretBackend,
		SecretRef:         req.SecretRef,
		Protocol:          req.Protocol,
		UpstreamURL:       req.UpstreamURL,
		Enabled:           req.Enabled,
		AddedAt:           time.Now().UTC(),
	}

	// Merge the new provider into the loaded config and validate before any
	// disk write. We persist the full merged config so existing providers are
	// preserved on success and easy to restore on probe failure (we just
	// re-delete the staged provider and re-save).
	cfg.Providers[req.Name] = v
	if err := cfg.Validate(); err != nil {
		delete(cfg.Providers, req.Name)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeAddFailed, err)
	}

	// The probe re-uses the normal secrets.Keyget path, which loads
	// providers.toml internally. Persist the staged config so Keyget can
	// find it, then roll back providers.toml + secret if the probe fails.
	if err := config.Save(cfg); err != nil {
		delete(cfg.Providers, req.Name)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeConfigSaveFailed, err)
	}

	// Synchronous probe so a bad key fails the add (not silently later). Failure
	// here rolls back providers.toml + the inline secret. A codex provider skips it:
	// its /v1/models is served by a lazily-started daemon, and starting that daemon
	// under the providers-config lock is forbidden (the proxy lock must not nest with
	// it). Instead the cache is seeded with codex's static model list so a fresh
	// entry carries the real models (not zero) and model resolution works at once.
	var fetched []models.Model
	if v.EffectiveProtocol() == config.ProtocolCodexOAuth {
		for _, id := range codexproxy.StaticModels() {
			fetched = append(fetched, models.Model{ID: id})
		}
	} else {
		probeCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		f, fetchErr := models.Fetch(probeCtx, v)
		if fetchErr != nil {
			// Roll back the staged provider row before returning the typed error.
			delete(cfg.Providers, req.Name)
			_ = config.Save(cfg)
			_ = rollbackInlineSecret(req)
			return nil, opErr(classifyAddErr(fetchErr), fetchErr)
		}
		fetched = f
	}

	// All good — write the profile JSON and refresh the models cache so the
	// user can immediately `cc-fleet models <provider>` without an explicit
	// `cc-fleet refresh`.
	path, err := profile.WriteForProvider(v, "")
	if err != nil {
		delete(cfg.Providers, req.Name)
		_ = config.Save(cfg)
		_ = rollbackInlineSecret(req)
		return nil, opErr(CodeProfileWriteFailed, err)
	}

	if err := updateModelsCache(req.Name, v.ModelsEndpoint, fetched); err != nil {
		// Cache write failure is non-fatal: the provider is fully staged and the
		// cache repopulates on the next `cc-fleet refresh`. Warn on stderr (so a
		// --json caller's stdout envelope stays clean) but don't fail the add.
		// The error is a cache/file error, never a key.
		fmt.Fprintf(os.Stderr, "warning: provider %s added but models cache update failed: %v\n", req.Name, err)
	}

	return &AddResult{
		Provider:    req.Name,
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
		return CodeProviderUnreachable
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
func updateModelsCache(provider, endpoint string, fetched []models.Model) error {
	cache, err := models.Load()
	if err != nil {
		return fmt.Errorf("load cache: %w", err)
	}
	if cache.Providers == nil {
		cache.Providers = map[string]*models.ProviderCache{}
	}
	cache.Providers[provider] = &models.ProviderCache{
		Provider:  provider,
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
	UpstreamURL    *string // openai-* real upstream base
	ModelsEndpoint *string
	DefaultModel   *string
	StrongModel    *string // "strong" capability slot ("" clears → follows DefaultModel)
	FastModel      *string // "fast" capability slot ("" clears → follows DefaultModel)
	Effort         *string // reasoning-effort level ("" clears); config.Validate rejects bad values
	DefaultPerm    *string // default permission mode for `cc-fleet run` ("" clears)
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
	// AddRequest.APIKey; only valid when the provider's effective
	// SecretBackend=="file" (other backends manage their own secrets).
	APIKey string
}

// EditResult mirrors the post-edit provider row (skill consumers parse this to
// surface the new values to the user without re-running list).
type EditResult struct {
	Provider *config.Provider `json:"provider"`
}

// Edit mutates the named provider in place. Only fields set in req are applied;
// the rest are preserved. A schema-violating edit is rolled back before any
// disk write. base_url changes also re-render the profile JSON so Claude
// Code picks up the new ANTHROPIC_BASE_URL on the next spawn.
//
// Runs under the global providers-config flock.
func Edit(req EditRequest) (*EditResult, error) {
	return withProvidersLock(func() (*EditResult, error) { return editLocked(req) })
}

func editLocked(req EditRequest) (*EditResult, error) {
	if err := ValidateProviderName(req.Name); err != nil {
		return nil, opErr(CodeProviderNameInvalid, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	v, ok := cfg.Providers[req.Name]
	if !ok {
		return nil, opErr(CodeProviderUnknown,
			fmt.Errorf("provider %q not in providers.toml", req.Name))
	}

	baseURLChanged := false
	if req.BaseURL != nil && *req.BaseURL != v.BaseURL {
		v.BaseURL = *req.BaseURL
		baseURLChanged = true
	}
	if req.UpstreamURL != nil {
		v.UpstreamURL = *req.UpstreamURL
	}
	if req.ModelsEndpoint != nil {
		v.ModelsEndpoint = *req.ModelsEndpoint
	}
	profileFieldChanged := false
	if req.DefaultModel != nil && *req.DefaultModel != v.DefaultModel {
		v.DefaultModel = *req.DefaultModel
		profileFieldChanged = true
	}
	if req.StrongModel != nil && *req.StrongModel != v.StrongModel {
		v.StrongModel = *req.StrongModel
		profileFieldChanged = true
	}
	if req.FastModel != nil && *req.FastModel != v.FastModel {
		v.FastModel = *req.FastModel
		profileFieldChanged = true
	}
	if req.Effort != nil && *req.Effort != v.Effort {
		v.Effort = *req.Effort // config.Validate rejects a bad value
		profileFieldChanged = true
	}
	if req.DefaultPerm != nil {
		v.DefaultPermission = *req.DefaultPerm // run-only; does not affect the profile
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
	// user can rotate a key using the provider's existing secret_ref without
	// re-passing --secret-ref. Non-file backends provision keys themselves.
	if req.APIKey != "" {
		if v.SecretBackend != "file" {
			return nil, opErr(CodeBackendUnsupported,
				fmt.Errorf("--api-key is only supported with secret backend file (provider %q uses %q)", req.Name, v.SecretBackend))
		}
		// Multi-key guard: once a provider has a keys.json, the inline single-key
		// path would silently write to the now-ignored legacy file. Refuse and
		// point at the TUI (the multi-key human entry point). The message carries
		// no key bytes.
		multi, mErr := secrets.IsMultiKey(req.Name)
		if mErr != nil {
			return nil, opErr(CodeConfigLoadFailed, fmt.Errorf("check multi-key state: %w", mErr))
		}
		if multi {
			return nil, opErr(CodeBackendUnsupported,
				fmt.Errorf("provider %q has multiple keys; manage them in the TUI (cc-fleet)", req.Name))
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

	// Write the rotated key BEFORE saving providers.toml: key-first means a later
	// Save failure leaves the config still pointing at the OLD (intact) ref so
	// the provider keeps working; save-first would point at a new ref whose key
	// doesn't exist yet, breaking the provider if the write then failed.
	if req.APIKey != "" {
		if err := writeFileSecret(v.SecretRef, []byte(req.APIKey)); err != nil {
			return nil, opErr(CodeSecretWriteFailed, err)
		}
	}

	if err := config.Save(cfg); err != nil {
		return nil, opErr(CodeConfigSaveFailed, err)
	}

	// Re-render the profile when a field it embeds moved: base_url
	// (ANTHROPIC_BASE_URL) or a model/effort field (the tier env + effortLevel).
	// The apiKeyHelper path is provider-independent, and default_permission is
	// run-only, so neither touches the profile.
	if baseURLChanged || profileFieldChanged {
		if _, err := profile.WriteForProvider(v, ""); err != nil {
			return nil, opErr(CodeProfileWriteFailed, err)
		}
	}

	return &EditResult{Provider: v}, nil
}

// ---------------------------------------------------------------------------
// remove
// ---------------------------------------------------------------------------

// RemoveRequest is the typed input for Remove.
type RemoveRequest struct {
	Name       string
	KeepSecret bool // when true, a file-backend secret or a codex own-login is preserved
}

// RemoveResult is the structured result of Remove.
type RemoveResult struct {
	Provider       string `json:"removed"`
	SecretRemoved  bool   `json:"secret_removed"`
	ProfileRemoved bool   `json:"profile_removed"`
	DefaultCleared bool   `json:"default_cleared,omitempty"` // the removed provider was the default_provider
}

// Remove deletes the provider row from providers.toml, the per-provider profile JSON,
// and (without --keep-secret) the credential it owns: a file-backend on-disk secret,
// or — for a codex provider — cc-fleet's own login token + its daemon. ~/.codex and
// other external backends are never auto-purged; the user clears those themselves.
//
// Runs under the global providers-config flock.
func Remove(req RemoveRequest) (*RemoveResult, error) {
	var codexRef string
	res, err := withProvidersLock(func() (*RemoveResult, error) {
		r, ref, e := removeLocked(req)
		codexRef = ref
		return r, e
	})
	if err != nil {
		return res, err
	}
	// Drop a removed codex provider's cc-fleet login (token file) + its daemon here,
	// OUTSIDE the providers lock: codexproxy's token/proxy locks are standalone scopes
	// that must never nest under it. LogoutIfUnreferenced no-ops if a concurrent re-add
	// reclaimed the credential, and surfaces a removal failure (a surviving login would
	// be silently reused on the next add). ~/.codex (the codex CLI login) is untouched.
	if codexRef != "" && !req.KeepSecret {
		if err := codexproxy.LogoutIfUnreferenced(codexRef); err != nil {
			return res, opErr(CodeSecretRemoveFailed, fmt.Errorf("remove codex login: %w", err))
		}
	}
	return res, nil
}

func removeLocked(req RemoveRequest) (*RemoveResult, string, error) {
	if err := ValidateProviderName(req.Name); err != nil {
		return nil, "", opErr(CodeProviderNameInvalid, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, "", opErr(CodeConfigLoadFailed, err)
	}
	v, ok := cfg.Providers[req.Name]
	if !ok {
		return nil, "", opErr(CodeProviderUnknown,
			fmt.Errorf("provider %q not in providers.toml", req.Name))
	}

	res := &RemoveResult{Provider: req.Name}
	// A codex provider's login is removed after the lock (see Remove); capture its
	// credential ref before the row is gone.
	codexRef := ""
	if v.EffectiveProtocol() == config.ProtocolCodexOAuth {
		codexRef = v.SecretRef
	}

	// Commit the config row deletion FIRST, before any destructive profile/secret
	// cleanup: if Save fails, providers.toml is unchanged and the profile + secret
	// are still intact — never a config row pointing at already-deleted artifacts.
	// Only once the row is durably gone do we reap the now-unreferenced profile +
	// secret; a failure there leaves a harmless orphan, never a dangling
	// reference or destroyed key the config still claims exists. (v still points
	// at the removed Provider struct — the map entry is gone but the value is held.)
	delete(cfg.Providers, req.Name)
	// Scrub a dangling default pointer in the SAME save: a default_provider naming
	// the removed row would otherwise survive on disk (use-time errors, not a Load
	// brick, but still a surprise).
	if cfg.DefaultProvider == req.Name {
		cfg.DefaultProvider = ""
		res.DefaultCleared = true
	}
	if err := config.Save(cfg); err != nil {
		return nil, "", opErr(CodeConfigSaveFailed, err)
	}

	// Profile removal is idempotent (RemoveForProvider swallows ENOENT).
	if err := profile.RemoveForProvider(req.Name); err != nil {
		return nil, "", opErr(CodeProfileWriteFailed, err)
	}
	res.ProfileRemoved = true

	// Secret cleanup: only file backend is auto-purged, and only when
	// the user didn't ask to keep it.
	if v.SecretBackend == "file" && !req.KeepSecret {
		if v.SecretRef != "" {
			if err := removeFileSecret(v.SecretRef); err != nil {
				return nil, "", opErr(CodeSecretRemoveFailed, err)
			}
			res.SecretRemoved = true
		}
		// Also purge the multi-key store + rotation counter (best-effort; a
		// missing file is not an error). Independent of secret_ref because a
		// multi-key provider's keys live in <provider>.keys.json, not secret_ref.
		if err := secrets.RemoveKeySet(req.Name); err != nil {
			return nil, "", opErr(CodeSecretRemoveFailed, err)
		}
	}

	return res, codexRef, nil
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

// ProviderView is the per-provider JSON shape `cc-fleet list --json` emits. Kept
// flat (no nested objects) so jq dispatch in the skill stays trivial.
type ProviderView struct {
	Name           string `json:"name"`
	BaseURL        string `json:"base_url"`
	DefaultModel   string `json:"default_model"`
	StrongModel    string `json:"strong_model,omitempty"` // blank → follows default_model
	FastModel      string `json:"fast_model,omitempty"`   // blank → follows default_model
	Effort         string `json:"effort,omitempty"`       // reasoning-effort level; blank → unset
	DefaultPerm    string `json:"default_permission,omitempty"`
	ModelsEndpoint string `json:"models_endpoint"`
	SecretBackend  string `json:"secret_backend"`
	SecretRef      string `json:"secret_ref"`
	Protocol       string `json:"protocol"`     // resolved wire class: "" | openai-chat | openai-responses | codex-oauth
	UpstreamURL    string `json:"upstream_url"` // real OpenAI base for openai-* (base_url is the loopback daemon)
	Enabled        bool   `json:"enabled"`
	ModelsCount    int    `json:"models_count"`
	ModelsStale    bool   `json:"models_stale"`
	Default        bool   `json:"default"` // this row is the EFFECTIVE default (configured, or sole-enabled auto)
}

// ListResult is the structured result of List. Providers is always non-nil even
// when empty so JSON consumers can iterate without a presence check.
type ListResult struct {
	Providers []ProviderView `json:"providers"`
	// DefaultProvider is the CONFIGURED default ("" = unset). When unset and exactly
	// one provider is enabled, that provider's row still carries Default=true (the
	// implicit "auto" default), so a consumer can read default_provider to know
	// whether it was pinned vs auto-resolved.
	DefaultProvider string `json:"default_provider"`
}

// List enumerates all configured providers in alphabetical order. Each row is
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

	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	// The effective default (configured, else sole-enabled) drives the per-row flag;
	// a resolution error (none/ambiguous) just means no row is flagged.
	effDefault, _, _ := cfg.ResolveProvider("")

	out := &ListResult{Providers: []ProviderView{}, DefaultProvider: cfg.DefaultProvider}
	for _, name := range names {
		v := cfg.Providers[name]
		view := ProviderView{
			Name:           v.Name,
			BaseURL:        v.BaseURL,
			DefaultModel:   v.DefaultModel,
			StrongModel:    v.StrongModel,
			FastModel:      v.FastModel,
			Effort:         v.Effort,
			DefaultPerm:    v.DefaultPermission,
			ModelsEndpoint: v.ModelsEndpoint,
			SecretBackend:  v.SecretBackend,
			SecretRef:      v.SecretRef,
			Protocol:       v.EffectiveProtocol(),
			UpstreamURL:    v.UpstreamURL,
			Enabled:        v.Enabled,
			Default:        name == effDefault,
		}
		if vc, ok := cache.Providers[name]; ok && vc != nil {
			view.ModelsCount = len(vc.Models)
			view.ModelsStale = models.IsStale(vc)
		} else {
			// No cache entry = stale by convention so the user knows a
			// refresh is needed.
			view.ModelsStale = true
		}
		out.Providers = append(out.Providers, view)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// default provider
// ---------------------------------------------------------------------------

// DefaultProviderView is the structured result of `cc-fleet default` (show). Source
// is "configured" (pinned + resolvable), "auto" (unset but a sole enabled provider
// serves), "disabled" (pinned but the provider is disabled), "unknown" (pinned but
// the provider no longer exists), or "unset" (nothing pinned, no sole provider).
// Provider is the effective/pinned name ("" only when truly unset). Candidates lists
// the enabled providers (the set to pin / ask among).
type DefaultProviderView struct {
	Provider   string   `json:"provider"`
	Source     string   `json:"source"`
	Configured string   `json:"configured"` // the pinned value ("" = not pinned)
	Candidates []string `json:"candidates"`
}

// DefaultProvider reports the effective default. Read-only. A pinned-but-broken
// default (disabled / removed) is reported as such — NOT "unset" — so the show
// command can suggest the right recovery (re-enable / --force / --unset) instead of
// a bare set that would fail DEFAULT_ALREADY_SET.
func DefaultProvider() (*DefaultProviderView, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	view := &DefaultProviderView{Configured: cfg.DefaultProvider, Candidates: cfg.EnabledProviders()}
	name, source, rerr := cfg.ResolveProvider("")
	switch {
	case rerr == nil && source == "default":
		view.Provider, view.Source = name, "configured"
	case rerr == nil && source == "sole":
		view.Provider, view.Source = name, "auto"
	case errors.Is(rerr, config.ErrDefaultProviderDisabled):
		view.Provider, view.Source = cfg.DefaultProvider, "disabled"
	case errors.Is(rerr, config.ErrDefaultProviderUnknown):
		view.Provider, view.Source = cfg.DefaultProvider, "unknown"
	default:
		view.Source = "unset"
	}
	return view, nil
}

// SetDefaultProvider pins default_provider. It refuses to overwrite an existing
// pin unless force is set (DEFAULT_ALREADY_SET) — a guard the skill relies on to
// only ever fill a blank default, never silently change the user's choice. The
// provider must exist (UNKNOWN); a disabled provider is allowed to be pinned (it
// errors at use, and the user may be pinning ahead of re-enabling).
func SetDefaultProvider(name string, force bool) (*DefaultProviderView, error) {
	_, err := withProvidersLock(func() (struct{}, error) {
		cfg, err := config.Load()
		if err != nil {
			return struct{}{}, opErr(CodeConfigLoadFailed, err)
		}
		if err := ValidateProviderName(name); err != nil {
			return struct{}{}, opErr(CodeProviderNameInvalid, err)
		}
		if _, ok := cfg.Providers[name]; !ok {
			return struct{}{}, opErr(CodeProviderUnknown, fmt.Errorf("provider %q not in providers.toml", name))
		}
		if cfg.DefaultProvider != "" && cfg.DefaultProvider != name && !force {
			return struct{}{}, opErr(CodeDefaultAlreadySet,
				fmt.Errorf("default provider is already %q; pass --force to change it", cfg.DefaultProvider))
		}
		cfg.DefaultProvider = name
		if err := config.Save(cfg); err != nil {
			return struct{}{}, opErr(CodeConfigSaveFailed, err)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return DefaultProvider()
}

// UnsetDefaultProvider clears default_provider (a no-op if already unset).
func UnsetDefaultProvider() (*DefaultProviderView, error) {
	_, err := withProvidersLock(func() (struct{}, error) {
		cfg, err := config.Load()
		if err != nil {
			return struct{}{}, opErr(CodeConfigLoadFailed, err)
		}
		if cfg.DefaultProvider != "" {
			cfg.DefaultProvider = ""
			if err := config.Save(cfg); err != nil {
				return struct{}{}, opErr(CodeConfigSaveFailed, err)
			}
		}
		return struct{}{}, nil
	})
	if err != nil {
		return nil, err
	}
	return DefaultProvider()
}

// ---------------------------------------------------------------------------
// repair
// ---------------------------------------------------------------------------

// RepairResult is the structured result of Repair.
type RepairResult struct {
	Repaired []string `json:"repaired"`
}

// Repair re-writes every provider's profile JSON from the current providers.toml.
// Secrets are NOT touched (Repair fixes profiles users may have accidentally
// deleted; secret backends own their own state).
func Repair() (*RepairResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)

	res := &RepairResult{Repaired: []string{}}
	for _, name := range names {
		v := cfg.Providers[name]
		if _, err := profile.WriteForProvider(v, ""); err != nil {
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
	KeepSecrets bool // default true at the caller layer (false under All)
	All         bool // also remove skills, the plugin, and the binary artifacts
}

// UninstallResult is the structured result of Uninstall. Manual lists the
// commands the user must run themselves to finish an All uninstall (tooling
// missing, unknown install method, or windows — a running exe can't unlink).
type UninstallResult struct {
	Removed []string `json:"removed"`
	Kept    []string `json:"kept"`
	Manual  []string `json:"manual,omitempty"`
}

// Uninstall removes every cc-fleet-owned state file: per-provider profile
// JSONs, providers.toml, fingerprint.json, models-cache.json, onboarding.json,
// the update-check cache, the team-history records under teams-history/, and
// finished background jobs under subagent-jobs/ (see subagent.PurgeJobs —
// finished job files are removed even when other jobs are still running; the
// live ones, and the dir itself, are kept and reported in Kept). Without All,
// the skill dirs (install machinery's), ~/.claude/teams/ (Claude Code's own
// state), the plugin, and the binary are preserved — a bare uninstall is a
// re-installable state reset.
//
// With All it is a complete uninstall: the cc-fleet skill dirs, the Claude
// Code plugin (via the claude CLI when available), and — last — the binary
// artifacts (method-aware via selfupdate) go too; whatever can't be done from
// inside this process lands in Manual as exact commands.
//
// Per-provider file-backend secrets are removed unless KeepSecrets is true (the
// caller-level default; under All the caller defaults it to false). The whole
// <SecretsDir>/ tree is removed only when KeepSecrets is false.
func Uninstall(req UninstallRequest) (*UninstallResult, error) {
	res := &UninstallResult{
		Removed: []string{},
		Kept:    []string{},
	}

	cfg, err := config.Load()
	if err != nil {
		// Treat a malformed providers.toml as "no providers known" — Uninstall
		// should still get the rest of the tree clean. Surface in Kept so
		// the user can see what was skipped.
		res.Kept = append(res.Kept, fmt.Sprintf("providers.toml (load failed: %v)", err))
		cfg = &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{}}
	}

	// 1. Per-provider profiles.
	for name := range cfg.Providers {
		path, perr := profile.ProfilePath(name)
		if perr != nil {
			// Per-provider failure shouldn't sink the whole uninstall.
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
		filepath.Join(cfgDir, "providers.toml"),
		filepath.Join(cfgDir, "fingerprint.json"),
		filepath.Join(cfgDir, "models-cache.json"),
	}
	// Decision/state files whose names are owned by other packages.
	if p, oerr := onboarding.StatePath(); oerr == nil {
		tops = append(tops, p)
	} else {
		res.Kept = append(res.Kept, fmt.Sprintf("onboarding.json (resolve failed: %v)", oerr))
	}
	if p, uerr := selfupdate.CheckCachePath(); uerr == nil {
		tops = append(tops, p)
	} else {
		res.Kept = append(res.Kept, fmt.Sprintf("update-check.json (resolve failed: %v)", uerr))
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

	// 2c-bis. Pin registry (ConfigDir/pinned). User "keep" markers for board records;
	// a full uninstall drops them too. Routed through pinned.Purge so the dir name stays
	// owned by that package.
	if pinPath, perr := pinned.Purge(); perr != nil {
		res.Kept = append(res.Kept, fmt.Sprintf("%s (purge failed: %v)", pinPath, perr))
	} else {
		res.Removed = append(res.Removed, pinPath)
	}

	// 2d. Codex proxy daemon + its state files. Routed through codexproxy.Purge
	// so the file names stay owned by that package; the daemon is stopped first.
	// The login token chain is a credential, so it follows KeepSecrets.
	cpRemoved, cpKept := codexproxy.Purge(req.KeepSecrets)
	res.Removed = append(res.Removed, cpRemoved...)
	res.Kept = append(res.Kept, cpKept...)

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

	// 4. Skills / plugin / binary. A bare uninstall preserves them (the skill
	// dirs are the install machinery's; ~/.claude/teams/ is Claude Code's own
	// state) and surfaces them in Kept so users know we considered them. All
	// removes everything cc-fleet's installers put on disk — binary last, so
	// the rest of the uninstall is already done when it goes.
	home, herr := homedir.Home()
	if herr != nil {
		res.Kept = append(res.Kept, fmt.Sprintf("skills/binary (resolve home failed: %v)", herr))
		return res, nil
	}
	skillsRoot := filepath.Join(home, ".claude", "skills")
	if teamsDir := filepath.Join(home, ".claude", "teams"); fileExists(teamsDir) {
		res.Kept = append(res.Kept, teamsDir) // always Claude Code's, never touched
	}
	if !req.All {
		if fileExists(skillsRoot) {
			res.Kept = append(res.Kept, skillsRoot)
		}
		return res, nil
	}

	// 4b. (All) the cc-fleet skill dirs — per-lane + shared + the legacy single
	// skill; never the skills root itself (other skills live there).
	for _, name := range []string{"cc-fleet-subagent", "cc-fleet-team", "cc-fleet-workflow", "cc-fleet-shared", "cc-fleet"} {
		dir := filepath.Join(skillsRoot, name)
		if !fileExists(dir) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			res.Kept = append(res.Kept, fmt.Sprintf("%s (rm -rf failed: %v)", dir, err))
		} else {
			res.Removed = append(res.Removed, dir)
		}
	}

	// 4c. (All) plugin, then the binary last, under the self-update lock so a
	// concurrent `ccf update` never mutates the same artifacts mid-removal.
	// Admin-tool output streams to stderr so a --json caller's stdout stays
	// machine-clean. The timeout bounds a hung claude/npm child — state is
	// already removed by now, so a stalled tool must degrade to manual
	// commands, not hang the uninstall.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if lerr := selfupdate.WithUpdateLock(func() error {
		pRemoved, pManual := selfupdate.UninstallPlugin(ctx, os.Stderr)
		res.Removed = append(res.Removed, pRemoved...)
		res.Manual = append(res.Manual, pManual...)

		bRemoved, bKept, bManual := selfupdate.UninstallBinary(ctx, os.Stderr)
		res.Removed = append(res.Removed, bRemoved...)
		res.Kept = append(res.Kept, bKept...)
		res.Manual = append(res.Manual, bManual...)
		return nil
	}); lerr != nil {
		res.Kept = append(res.Kept, fmt.Sprintf("plugin/binary (update lock: %v)", lerr))
	}

	return res, nil
}

// fileExists reports whether path exists (any type), without following a
// trailing symlink.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
