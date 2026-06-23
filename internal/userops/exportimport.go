package userops

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
)

// ExportRequest selects which providers to export; an empty Providers list exports all
// non-codex providers.
type ExportRequest struct {
	Providers []string
}

// ExportResult is a keyless provider bundle plus a record of what travelled and what was
// omitted (codex rows have no portable credential).
type ExportResult struct {
	Bundle       []byte   `json:"-"`
	Written      []string `json:"written"`
	OmittedCodex []string `json:"omitted_codex"`
}

// Export builds a keyless, versioned TOML bundle of the provider roster + the global
// default. codex providers are omitted (their auth is a machine-local login, not a
// portable reference); a daemon-backed provider's loopback base_url is dropped (it is
// re-derived on import). No key material is read.
func Export(req ExportRequest) (*ExportResult, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, opErr(CodeConfigLoadFailed, err)
	}

	want := map[string]bool{}
	for _, n := range req.Providers {
		if _, ok := cfg.Providers[n]; !ok {
			return nil, opErr(CodeProviderUnknown, fmt.Errorf("no such provider %q", n))
		}
		want[n] = true
	}

	b := &config.Bundle{Version: config.BundleVersion, Providers: map[string]*config.Provider{}}
	res := &ExportResult{Written: []string{}, OmittedCodex: []string{}}
	for _, name := range sortedProviderNames(cfg.Providers) {
		if len(want) > 0 && !want[name] {
			continue
		}
		v := cfg.Providers[name]
		if v.EffectiveProtocol() == config.ProtocolCodexOAuth {
			res.OmittedCodex = append(res.OmittedCodex, name)
			continue
		}
		b.Providers[name] = portableCopy(v)
		res.Written = append(res.Written, name)
	}
	// Carry the default only if its target survived the filter/codex-omit.
	if cfg.DefaultProvider != "" {
		if _, ok := b.Providers[cfg.DefaultProvider]; ok {
			b.DefaultProvider = cfg.DefaultProvider
		}
	}

	data, err := config.MarshalBundle(b)
	if err != nil {
		return nil, opErr(CodeExportFailed, err)
	}
	res.Bundle = data
	return res, nil
}

// portableCopy strips a daemon-backed provider's loopback base_url (re-derived on the
// target). The key was never in the row; added_at is overwritten on import.
func portableCopy(v *config.Provider) *config.Provider {
	c := *v
	if c.DaemonBacked() {
		c.BaseURL = ""
	}
	return &c
}

// ImportRequest is a bundle to apply. Force overwrites colliding providers (after backing
// up providers.toml); without it, collisions are skipped.
type ImportRequest struct {
	Bundle []byte
	Force  bool
}

// ImportResult records the per-provider outcome, the default-provider result, and the
// backup path (set only when Force backed up an existing config).
type ImportResult struct {
	Added            []string `json:"added"`
	Overwritten      []string `json:"overwritten"`
	SkippedCollision []string `json:"skipped_collision"`
	SkippedCodex     []string `json:"skipped_codex"`
	DefaultSet       string   `json:"default_set,omitempty"`
	DefaultDropped   string   `json:"default_dropped,omitempty"`
	BackupPath       string   `json:"backup_path,omitempty"`
}

// Import merges a keyless bundle into providers.toml under the providers lock. It fully
// validates the merged candidate before a single atomic write — a bad bundle leaves the
// roster untouched. codex rows are skipped; colliding rows are skipped unless Force.
// A daemon-backed row's loopback base_url is re-derived with a batch-aware port allocator.
// It writes config only — profiles are rebuilt afterward by Repair.
func Import(req ImportRequest) (*ImportResult, error) {
	return withProvidersLock(func() (*ImportResult, error) {
		bundle, err := config.ParseBundle(req.Bundle)
		if err != nil {
			return nil, opErr(CodeImportFailed, err)
		}
		cfg, err := config.Load()
		if err != nil {
			return nil, opErr(CodeConfigLoadFailed, err)
		}

		res := &ImportResult{Added: []string{}, Overwritten: []string{}, SkippedCollision: []string{}, SkippedCodex: []string{}}

		// Pass 1: classify the rows we will actually apply (skip codex; skip collisions
		// unless Force), validating each file-backend secret_ref. (A hand-edited bundle
		// could carry a codex row even though export omits them.)
		type applyRow struct {
			p        *config.Provider
			existing *config.Provider // nil for a fresh add
		}
		apply := map[string]applyRow{}
		for _, name := range sortedProviderNames(bundle.Providers) {
			p := bundle.Providers[name]
			// The same per-provider persistence policy add enforces (reserved native
			// name + path-safe file secret_ref) — config.Validate alone misses these, so
			// share addLocked's guard. A [claude] row or an unsafe ref fails closed.
			if err := guardProviderForPersistence(name, p.SecretBackend, p.SecretRef); err != nil {
				return nil, err
			}
			if p.EffectiveProtocol() == config.ProtocolCodexOAuth {
				res.SkippedCodex = append(res.SkippedCodex, name)
				continue
			}
			existing, collision := cfg.Providers[name]
			if collision && !req.Force {
				res.SkippedCollision = append(res.SkippedCollision, name)
				continue
			}
			ar := applyRow{p: p}
			if collision {
				ar.existing = existing
			}
			apply[name] = ar
		}

		// Reserve loopback ports BEFORE allocating any new one, so an addition can never
		// grab a port that a same-name daemon-backed overwrite will reuse. A surviving
		// daemon-backed provider keeps its port; a port is freed only when its existing
		// daemon row is overwritten by a NON-daemon-backed row.
		reserved := map[int]bool{}
		for name, v := range cfg.Providers {
			if !v.DaemonBacked() {
				continue
			}
			if ar, ok := apply[name]; ok && !ar.p.DaemonBacked() {
				continue
			}
			if port, e := codexproxy.PortFromBaseURL(v.BaseURL); e == nil {
				reserved[port] = true
			}
		}

		// Pass 2: apply each row, re-deriving a loopback base_url for daemon-backed rows.
		for _, name := range sortedProviderNames(bundle.Providers) {
			ar, ok := apply[name]
			if !ok {
				continue // skipped in pass 1
			}
			p := ar.p
			if p.DaemonBacked() {
				port := 0
				if ar.existing != nil && ar.existing.DaemonBacked() {
					if pp, e := codexproxy.PortFromBaseURL(ar.existing.BaseURL); e == nil {
						port = pp // reuse the overwritten row's own port (already reserved)
					}
				}
				if port == 0 {
					if port, err = codexproxy.ChoosePortBatch(reserved); err != nil {
						return nil, opErr(CodeImportFailed, fmt.Errorf("provider %q: %w", name, err))
					}
				}
				reserved[port] = true
				p.BaseURL = fmt.Sprintf("http://127.0.0.1:%d/", port)
			}
			if ar.existing != nil {
				p.AddedAt = ar.existing.AddedAt // keep the original add time on overwrite
				res.Overwritten = append(res.Overwritten, name)
			} else {
				p.AddedAt = time.Now()
				res.Added = append(res.Added, name)
			}
			cfg.Providers[name] = p
		}

		// Restore the default under the same policy SetDefaultProvider enforces (shared via
		// defaultChangeAllowed): never the reserved leaf, the target must survive, and an
		// existing different default is not silently repinned without --force. If the policy
		// refuses, the bundle default is dropped rather than failing the whole import.
		if d := bundle.DefaultProvider; d != "" {
			if err := defaultChangeAllowed(cfg, d, req.Force); err != nil {
				res.DefaultDropped = d
			} else {
				cfg.DefaultProvider = d
				res.DefaultSet = d
			}
		}

		// The single strict gate over the merged roster — a bad bundle is rejected here,
		// before any write.
		if err := cfg.Validate(); err != nil {
			return nil, opErr(CodeImportFailed, err)
		}

		// Back up the existing config before a forced overwrite.
		if req.Force {
			if bp, err := backupProviders(); err != nil {
				return nil, opErr(CodeImportFailed, err)
			} else if bp != "" {
				res.BackupPath = bp
			}
		}

		if err := config.Save(cfg); err != nil {
			return nil, opErr(CodeConfigSaveFailed, err)
		}
		return res, nil
	})
}

// backupProviders copies the current providers.toml to a timestamped sibling. It returns
// "" (no error) when there is no existing config to back up.
func backupProviders() (string, error) {
	path, err := config.ProvidersPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read providers.toml for backup: %w", err)
	}
	// O_EXCL create-temp so rapid successive --force imports never clobber an earlier
	// backup (a second-granularity timestamp alone would collide).
	f, err := os.CreateTemp(filepath.Dir(path), fmt.Sprintf("providers.toml.bak-%d-*", time.Now().Unix()))
	if err != nil {
		return "", fmt.Errorf("create backup: %w", err)
	}
	name := f.Name()
	fail := func(e error) (string, error) {
		_ = f.Close()
		_ = os.Remove(name)
		return "", e
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("chmod backup: %w", err))
	}
	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("write backup: %w", err))
	}
	// Close explicitly and treat a flush error as a failed backup — this is the only
	// automatic recovery copy for a --force import, so it must be durable before we save.
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("flush backup: %w", err)
	}
	return name, nil
}

func sortedProviderNames(m map[string]*config.Provider) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
