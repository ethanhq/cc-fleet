package userops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/secrets"
)

// setupHome points HOME (and clears XDG_CONFIG_HOME) at a fresh temp dir so
// every test's filesystem effects are isolated. Returns the dir for any test
// that wants to inspect on-disk state.
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// seedVendor writes a vendor row directly into vendors.toml without running
// Add (no probe). Used by tests that exercise paths after a vendor exists.
func seedVendor(t *testing.T, name string) *config.Vendor {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("seedVendor: config.Load: %v", err)
	}
	v := &config.Vendor{
		Name:           name,
		BaseURL:        "https://api." + name + ".example/anthropic",
		ModelsEndpoint: "https://api." + name + ".example/v1/models",
		DefaultModel:   name + "-flash",
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Enabled:        true,
		AddedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	cfg.Vendors[name] = v
	if err := config.Save(cfg); err != nil {
		t.Fatalf("seedVendor: config.Save: %v", err)
	}
	return v
}

// seedSecretFile creates a fake file-backend secret on disk so Remove tests
// have something to delete (or keep, per --keep-secret).
func seedSecretFile(t *testing.T, ref, body string) string {
	t.Helper()
	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("seedSecretFile: SecretsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seedSecretFile: MkdirAll: %v", err)
	}
	path := filepath.Join(dir, ref)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seedSecretFile: WriteFile: %v", err)
	}
	return path
}

func TestInit_CreatesTreeOnce(t *testing.T) {
	home := setupHome(t)
	res, err := Init()
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// First run must have created the top-level config + secrets + profiles + skill dir.
	wantDirs := []string{
		filepath.Join(home, ".config", "cc-fleet"),
		filepath.Join(home, ".config", "cc-fleet", "secrets"),
		filepath.Join(home, ".claude", "profiles"),
		filepath.Join(home, ".claude", "skills", "cc-fleet"),
		filepath.Join(home, ".config", "cc-fleet", "vendors.toml"),
	}
	for _, p := range wantDirs {
		if !contains(res.Created, p) {
			t.Fatalf("Init Created missing %q (got %v)", p, res.Created)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if runtime.GOOS != "windows" {
			// vendors.toml should be 0600; the directories should be 0700.
			if filepath.Base(p) == "vendors.toml" {
				if got := info.Mode().Perm(); got != 0o600 {
					t.Fatalf("%s mode = %o, want 0600", p, got)
				}
			} else if got := info.Mode().Perm(); got != 0o700 {
				t.Fatalf("%s mode = %o, want 0700", p, got)
			}
		}
	}

	// Second run must be a no-op: every entry should land in AlreadyHad.
	res2, err := Init()
	if err != nil {
		t.Fatalf("Init (re-run): %v", err)
	}
	if len(res2.Created) != 0 {
		t.Fatalf("Init re-run Created = %v, want empty", res2.Created)
	}
	for _, p := range wantDirs {
		if !contains(res2.AlreadyHad, p) {
			t.Fatalf("Init re-run AlreadyHad missing %q (got %v)", p, res2.AlreadyHad)
		}
	}
}

func TestInit_PreservesExistingVendorsTOML(t *testing.T) {
	// Init must NOT overwrite vendors.toml if one already exists — that
	// would destroy user-added vendors on every run.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v := seedVendor(t, "glm")
	if _, err := Init(); err != nil {
		t.Fatalf("Init (re-run with vendor): %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load after re-init: %v", err)
	}
	got, ok := cfg.Vendors["glm"]
	if !ok {
		t.Fatalf("vendor glm vanished after Init")
	}
	if got.BaseURL != v.BaseURL {
		t.Fatalf("vendor mutated: got %+v, want %+v", got, v)
	}
}

func TestAdd_RejectsDuplicate(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")

	_, err := Add(AddRequest{
		Name:           "glm",
		BaseURL:        "https://x.example",
		ModelsEndpoint: "https://x.example/v1/models",
		DefaultModel:   "x",
		SecretBackend:  "file",
		SecretRef:      "glm.key",
	})
	if err == nil {
		t.Fatalf("Add duplicate: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("err type = %T, want *Op", err)
	}
	if op.Code != CodeVendorExists {
		t.Fatalf("err code = %q, want %q", op.Code, CodeVendorExists)
	}
}

func TestAdd_RejectsBadName(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := Add(AddRequest{
		Name:           "bad/name",
		BaseURL:        "https://x.example",
		ModelsEndpoint: "https://x.example/v1/models",
		DefaultModel:   "x",
		SecretBackend:  "file",
		SecretRef:      "x.key",
	})
	if err == nil {
		t.Fatalf("Add bad-name: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("err type = %T, want *Op", err)
	}
	if op.Code != CodeVendorNameInvalid {
		t.Fatalf("err code = %q, want %q", op.Code, CodeVendorNameInvalid)
	}
}

func TestAdd_RejectsAPIKeyForNonFileBackend(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := Add(AddRequest{
		Name:           "glm",
		BaseURL:        "https://x.example",
		ModelsEndpoint: "https://x.example/v1/models",
		DefaultModel:   "x",
		SecretBackend:  "pass",
		SecretRef:      "glm/key",
		APIKey:         "sk-something",
	})
	if err == nil {
		t.Fatalf("Add pass+APIKey: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("err type = %T, want *Op", err)
	}
	if op.Code != CodeBackendUnsupported {
		t.Fatalf("err code = %q, want %q", op.Code, CodeBackendUnsupported)
	}
}

// TestAdd_RejectsUnsafeSecretRef is the write side: a file-backend secret_ref
// that would escape SecretsDir is rejected before any file is written (clean
// CodeInvalidBackend, no traversal write).
func TestAdd_RejectsUnsafeSecretRef(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := Add(AddRequest{
		Name:           "glm",
		BaseURL:        "https://api.glm.example/anthropic",
		ModelsEndpoint: "https://api.glm.example/v1/models",
		DefaultModel:   "glm-4.6",
		SecretBackend:  "file",
		SecretRef:      "../../etc/shadow",
		APIKey:         "sk-should-never-be-written",
		Enabled:        true,
	})
	if err == nil {
		t.Fatal("Add file-backend + traversal secret_ref: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) || op.Code != CodeInvalidBackend {
		t.Fatalf("Add err = %v, want *Op CodeInvalidBackend", err)
	}
	// No secret file may have been written (inside or outside the secrets dir):
	// the ref is rejected before writeFileSecret runs.
	dir, _ := config.SecretsDir()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("Add wrote %d secret file(s) for a rejected ref, want 0", len(entries))
	}
}

func TestEdit_VendorUnknown(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	model := "glm-4.5"
	_, err := Edit(EditRequest{Name: "ghost", DefaultModel: &model})
	if err == nil {
		t.Fatalf("Edit unknown vendor: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("err type = %T, want *Op", err)
	}
	if op.Code != CodeVendorUnknown {
		t.Fatalf("err code = %q, want %q", op.Code, CodeVendorUnknown)
	}
}

func TestEdit_AppliesOnlySetFields(t *testing.T) {
	// Edit should leave non-nil-pointer fields untouched, including the
	// AddedAt timestamp and unrelated URLs. Only the requested field moves.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	original := seedVendor(t, "glm")

	newModel := "glm-4.5"
	res, err := Edit(EditRequest{Name: "glm", DefaultModel: &newModel})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if res.Vendor.DefaultModel != newModel {
		t.Fatalf("DefaultModel = %q, want %q", res.Vendor.DefaultModel, newModel)
	}
	if res.Vendor.BaseURL != original.BaseURL {
		t.Fatalf("BaseURL mutated: got %q, want %q", res.Vendor.BaseURL, original.BaseURL)
	}
	if !res.Vendor.AddedAt.Equal(original.AddedAt) {
		t.Fatalf("AddedAt mutated: got %v, want %v", res.Vendor.AddedAt, original.AddedAt)
	}
}

func TestEdit_APIKeyRotatesFileSecret(t *testing.T) {
	// `cc-fleet edit <vendor> --api-key` rotates the key in place, writing to
	// the vendor's EXISTING secret_ref (no --secret-ref needed) and leaving
	// every other field untouched.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	orig := seedVendor(t, "glm") // file backend, SecretRef "glm.key"
	secretPath := seedSecretFile(t, orig.SecretRef, "old-key")

	res, err := Edit(EditRequest{Name: "glm", APIKey: "new-rotated-key"})
	if err != nil {
		t.Fatalf("Edit --api-key: %v", err)
	}

	// Key rotated to the existing secret_ref.
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read rotated secret: %v", err)
	}
	if string(got) != "new-rotated-key" {
		t.Fatalf("secret = %q, want the rotated key", string(got))
	}

	// Only --api-key was passed → every other field is preserved.
	if res.Vendor.BaseURL != orig.BaseURL ||
		res.Vendor.DefaultModel != orig.DefaultModel ||
		res.Vendor.ModelsEndpoint != orig.ModelsEndpoint ||
		res.Vendor.SecretBackend != orig.SecretBackend ||
		res.Vendor.SecretRef != orig.SecretRef ||
		res.Vendor.Enabled != orig.Enabled ||
		!res.Vendor.AddedAt.Equal(orig.AddedAt) {
		t.Fatalf("non-key fields mutated:\n got: %+v\nwant: %+v", res.Vendor, orig)
	}
}

func TestEdit_APIKeyRejectsNonFileBackend(t *testing.T) {
	// Inline --api-key is only legal for the file backend; a pass/vault/etc.
	// vendor must rotate through its own tool. Mirrors Add's same guard.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.Vendors["kp"] = &config.Vendor{
		Name:           "kp",
		BaseURL:        "https://api.kp.example/anthropic",
		ModelsEndpoint: "https://api.kp.example/v1/models",
		DefaultModel:   "kp-flash",
		SecretBackend:  "pass",
		SecretRef:      "kp/key",
		Enabled:        true,
		AddedAt:        time.Now().UTC(),
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	_, err = Edit(EditRequest{Name: "kp", APIKey: "sk-should-reject"})
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("Edit pass-backend + APIKey: want *Op error, got %v", err)
	}
	if op.Code != CodeBackendUnsupported {
		t.Fatalf("err code = %q, want %q", op.Code, CodeBackendUnsupported)
	}
}

func TestEdit_APIKeyEmptySecretRefRejected(t *testing.T) {
	// file backend + --api-key but the effective secret_ref is empty → reject
	// with CodeInvalidBackend (nowhere to write the key). We clear the ref in
	// the same edit that supplies the key.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm") // file backend, ref "glm.key"

	emptyRef := ""
	_, err := Edit(EditRequest{Name: "glm", SecretRef: &emptyRef, APIKey: "new-key"})
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("Edit file-backend + empty ref + APIKey: want *Op error, got %v", err)
	}
	if op.Code != CodeInvalidBackend {
		t.Fatalf("err code = %q, want %q", op.Code, CodeInvalidBackend)
	}
}

// TestEdit_RejectsUnsafeSecretRef mirrors the Add guard: rotating a key onto a
// traversal secret_ref is refused with CodeInvalidBackend.
func TestEdit_RejectsUnsafeSecretRef(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm") // file backend, ref "glm.key"

	bad := "../../etc/shadow"
	_, err := Edit(EditRequest{Name: "glm", SecretRef: &bad, APIKey: "sk-x"})
	var op *Op
	if !errors.As(err, &op) || op.Code != CodeInvalidBackend {
		t.Fatalf("Edit + traversal ref: err = %v, want *Op CodeInvalidBackend", err)
	}
}

func TestEdit_AppliesKeyRotation(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm") // file backend

	rr := "round_robin"
	res, err := Edit(EditRequest{Name: "glm", KeyRotation: &rr})
	if err != nil {
		t.Fatalf("Edit --key-rotation: %v", err)
	}
	if res.Vendor.KeyRotation != "round_robin" {
		t.Fatalf("result key_rotation = %q, want round_robin", res.Vendor.KeyRotation)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Vendors["glm"].KeyRotation != "round_robin" {
		t.Fatalf("persisted key_rotation = %q, want round_robin", cfg.Vendors["glm"].KeyRotation)
	}

	// An invalid strategy is rejected by config.Validate during Edit.
	bad := "rotate-fast"
	if _, err := Edit(EditRequest{Name: "glm", KeyRotation: &bad}); err == nil {
		t.Fatalf("Edit with invalid key_rotation: want error, got nil")
	}
}

func TestEdit_MultiKeyAPIKeyGuard(t *testing.T) {
	// A vendor in multi-key mode (keys.json present) must reject inline
	// --api-key with a clear, TUI-pointing error that carries no key bytes.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	if err := secrets.SaveKeySet("glm", []secrets.KeyEntry{
		{Label: "a", Key: "sk-aaa-111", Enabled: true},
		{Label: "b", Key: "sk-bbb-222", Enabled: true},
	}); err != nil {
		t.Fatalf("SaveKeySet: %v", err)
	}

	const inline = "sk-INLINE-SHOULD-BE-REFUSED-7777"
	_, err := Edit(EditRequest{Name: "glm", APIKey: inline})
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("Edit multi-key + APIKey: want *Op, got %v", err)
	}
	if op.Code != CodeBackendUnsupported {
		t.Fatalf("err code = %q, want %q", op.Code, CodeBackendUnsupported)
	}
	if strings.Contains(err.Error(), inline) {
		t.Fatalf("guard error leaked the inline key: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "TUI") {
		t.Fatalf("guard error should point at the TUI: %q", err.Error())
	}
}

func TestRemove_VendorUnknown(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_, err := Remove(RemoveRequest{Name: "ghost"})
	if err == nil {
		t.Fatalf("Remove unknown vendor: want error, got nil")
	}
	var op *Op
	if !errors.As(err, &op) {
		t.Fatalf("err type = %T, want *Op", err)
	}
	if op.Code != CodeVendorUnknown {
		t.Fatalf("err code = %q, want %q", op.Code, CodeVendorUnknown)
	}
}

func TestRemove_FileBackendDeletesSecret(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	secretPath := seedSecretFile(t, "glm.key", "sk-fake-glm")

	res, err := Remove(RemoveRequest{Name: "glm"})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !res.SecretRemoved {
		t.Fatalf("SecretRemoved = false, want true")
	}
	if !res.ProfileRemoved {
		t.Fatalf("ProfileRemoved = false, want true")
	}
	if _, err := os.Stat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret still exists at %s (err=%v)", secretPath, err)
	}

	// Vendor row must be gone from vendors.toml.
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load post-remove: %v", err)
	}
	if _, ok := cfg.Vendors["glm"]; ok {
		t.Fatalf("vendor glm still in vendors.toml after Remove")
	}
}

func TestRemove_KeepSecretPreservesFile(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	secretPath := seedSecretFile(t, "glm.key", "sk-keep-me")

	res, err := Remove(RemoveRequest{Name: "glm", KeepSecret: true})
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.SecretRemoved {
		t.Fatalf("SecretRemoved = true, want false when KeepSecret=true")
	}
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("secret missing at %s (want preserved): %v", secretPath, err)
	}
}

func TestRemove_CleansMultiKeyFiles(t *testing.T) {
	// Removing a file-backend vendor (without --keep-secret) also purges its
	// multi-key store and rotation counter.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	seedSecretFile(t, "glm.key", "legacy")
	if err := secrets.SaveKeySet("glm", []secrets.KeyEntry{{Key: "k", Enabled: true}}); err != nil {
		t.Fatalf("SaveKeySet: %v", err)
	}
	dir, _ := config.SecretsDir()
	keysPath := filepath.Join(dir, "glm.keys.json")
	rotPath := filepath.Join(dir, "glm.rotation")
	if err := os.WriteFile(rotPath, []byte("3"), 0o600); err != nil {
		t.Fatalf("seed rotation: %v", err)
	}

	if _, err := Remove(RemoveRequest{Name: "glm"}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, p := range []string{keysPath, rotPath} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed by Remove (err=%v)", p, err)
		}
	}
}

func TestRemove_KeepSecretPreservesMultiKeyFiles(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	if err := secrets.SaveKeySet("glm", []secrets.KeyEntry{{Key: "k", Enabled: true}}); err != nil {
		t.Fatalf("SaveKeySet: %v", err)
	}
	dir, _ := config.SecretsDir()
	keysPath := filepath.Join(dir, "glm.keys.json")

	if _, err := Remove(RemoveRequest{Name: "glm", KeepSecret: true}); err != nil {
		t.Fatalf("Remove --keep-secret: %v", err)
	}
	if _, err := os.Stat(keysPath); err != nil {
		t.Fatalf("keys.json should be preserved with --keep-secret: %v", err)
	}
}

func TestRemove_NonFileBackendNeverTouchesSecret(t *testing.T) {
	// pass / 1password / vault / keyring backends store their secret outside
	// SecretsDir; Remove must not delete anything in those cases (and
	// SecretRemoved must remain false).
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Vendors["passy"] = &config.Vendor{
		Name:           "passy",
		BaseURL:        "https://x.example",
		ModelsEndpoint: "https://x.example/v1/models",
		DefaultModel:   "x",
		SecretBackend:  "pass",
		SecretRef:      "passy/key",
		Enabled:        true,
		AddedAt:        time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	res, err := Remove(RemoveRequest{Name: "passy"})
	if err != nil {
		t.Fatalf("Remove pass-backend: %v", err)
	}
	if res.SecretRemoved {
		t.Fatalf("SecretRemoved = true; want false for non-file backend")
	}
}

func TestList_EmptyConfigReturnsArray(t *testing.T) {
	// list --json must emit a `vendors` array even when empty so jq dispatch in
	// the skill doesn't have to special-case nil.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	res, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Vendors == nil {
		t.Fatalf("List.Vendors = nil, want empty slice")
	}
	if len(res.Vendors) != 0 {
		t.Fatalf("List.Vendors = %v, want empty", res.Vendors)
	}
}

func TestList_PopulatedSortedByName(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "zeta")
	seedVendor(t, "alpha")
	seedVendor(t, "mu")

	res, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Vendors) != 3 {
		t.Fatalf("len = %d, want 3", len(res.Vendors))
	}
	got := []string{res.Vendors[0].Name, res.Vendors[1].Name, res.Vendors[2].Name}
	want := []string{"alpha", "mu", "zeta"}
	for i, n := range got {
		if n != want[i] {
			t.Fatalf("Vendors[%d].Name = %q, want %q (full: %v)", i, n, want[i], got)
		}
	}
	// No cache entries seeded → every vendor should be flagged stale=true.
	for _, vv := range res.Vendors {
		if !vv.ModelsStale {
			t.Fatalf("vendor %q ModelsStale = false; want true (no cache yet)", vv.Name)
		}
		if vv.ModelsCount != 0 {
			t.Fatalf("vendor %q ModelsCount = %d; want 0", vv.Name, vv.ModelsCount)
		}
	}
}

func TestRepair_RewritesAllProfiles(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	seedVendor(t, "deepseek")

	// Pre-delete glm.json so we can prove Repair recreated it.
	glmProfile, err := profile.ProfilePath("glm")
	if err != nil {
		t.Fatalf("ProfilePath: %v", err)
	}
	// Profile may or may not exist depending on prior steps — best-effort delete.
	_ = os.Remove(glmProfile)
	if _, err := os.Stat(glmProfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-condition: glm profile still exists at %s", glmProfile)
	}

	res, err := Repair()
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if len(res.Repaired) != 2 {
		t.Fatalf("Repaired = %v, want 2 entries", res.Repaired)
	}
	if !contains(res.Repaired, "glm") || !contains(res.Repaired, "deepseek") {
		t.Fatalf("Repaired = %v, want both glm and deepseek", res.Repaired)
	}
	info, err := os.Stat(glmProfile)
	if err != nil {
		t.Fatalf("glm profile missing after Repair: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("glm profile mode = %o, want 0600", got)
		}
	}
}

func TestUninstall_KeepSecretsDefault(t *testing.T) {
	// Default KeepSecrets=true preserves the secrets/ dir and the secret
	// files inside it. Profiles, vendors.toml, fingerprint, models cache
	// must all be removed.
	home := setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	secretPath := seedSecretFile(t, "glm.key", "sk-keep")
	// Seed a profile so Uninstall has something to clean up.
	v, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := profile.WriteForVendor(v.Vendors["glm"], "/usr/bin/cc-fleet"); err != nil {
		t.Fatalf("WriteForVendor: %v", err)
	}
	// Seed fingerprint.json + models-cache.json by writing empty files.
	cfgDir, _ := config.ConfigDir()
	for _, name := range []string{"fingerprint.json", "models-cache.json"} {
		p := filepath.Join(cfgDir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	res, err := Uninstall(UninstallRequest{KeepSecrets: true})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	// vendors.toml + fingerprint.json + models-cache.json + the glm profile
	// must all be in Removed.
	mustRemoved := []string{
		filepath.Join(cfgDir, "vendors.toml"),
		filepath.Join(cfgDir, "fingerprint.json"),
		filepath.Join(cfgDir, "models-cache.json"),
		filepath.Join(home, ".claude", "profiles", "glm.json"),
	}
	for _, p := range mustRemoved {
		if !contains(res.Removed, p) {
			t.Fatalf("Removed missing %q (got %v)", p, res.Removed)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists post-Uninstall (err=%v)", p, err)
		}
	}

	// Secrets dir must remain on disk (KeepSecrets=true).
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("secret missing after Uninstall --keep-secrets: %v", err)
	}
	secretsDir, _ := config.SecretsDir()
	if !contains(res.Kept, secretsDir) {
		t.Fatalf("Kept missing secrets dir (got %v)", res.Kept)
	}
}

func TestUninstall_WipeSecrets(t *testing.T) {
	// KeepSecrets=false should rm -rf the whole secrets/ tree.
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedVendor(t, "glm")
	secretPath := seedSecretFile(t, "glm.key", "sk-burn")

	res, err := Uninstall(UninstallRequest{KeepSecrets: false})
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	secretsDir, _ := config.SecretsDir()
	if !contains(res.Removed, secretsDir) {
		t.Fatalf("Removed missing secrets dir (got %v)", res.Removed)
	}
	if _, err := os.Stat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("secret still exists after Uninstall --no-keep-secrets (err=%v)", err)
	}
}

func TestUninstall_PreservesSkillAndTeamsDirs(t *testing.T) {
	// Per spec: ~/.claude/skills/cc-fleet/ and ~/.claude/teams/ must
	// remain after Uninstall (those are owned by other components).
	home := setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	teamsDir := filepath.Join(home, ".claude", "teams")
	if err := os.MkdirAll(teamsDir, 0o700); err != nil {
		t.Fatalf("MkdirAll teams: %v", err)
	}
	skillDir := filepath.Join(home, ".claude", "skills", "cc-fleet")
	// Init already created this; touch a marker file to make sure Uninstall
	// doesn't recursively delete the contents.
	marker := filepath.Join(skillDir, "PLACEHOLDER.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if _, err := Uninstall(UninstallRequest{KeepSecrets: true}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(skillDir); err != nil {
		t.Fatalf("skill dir wiped: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("skill content wiped: %v", err)
	}
	if _, err := os.Stat(teamsDir); err != nil {
		t.Fatalf("teams dir wiped: %v", err)
	}
}

// contains is the tiny string-slice helper the tests above lean on.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestClassifyAddErr_KeyInvalidSentinel makes sure classifyAddErr maps the
// models.ErrKeyInvalid sentinel onto CodeKeyInvalid even when wrapped — that
// dispatch is the only reason "bad key on add" returns the right error_code.
func TestClassifyAddErr_KeyInvalidSentinel(t *testing.T) {
	wrapped := fmt.Errorf("Fetch wrapper: %w", models.ErrKeyInvalid)
	if got := classifyAddErr(wrapped); got != CodeKeyInvalid {
		t.Fatalf("classifyAddErr(wrapped key invalid) = %q, want %q", got, CodeKeyInvalid)
	}
	if !strings.Contains(wrapped.Error(), "401") {
		t.Fatalf("wrapped err lost sentinel message: %v", wrapped)
	}
	// Non-sentinel errors fall through to ADD_FAILED unless they look like
	// transport errors (DNS / dial / deadline).
	if got := classifyAddErr(errors.New("random vendor returned 500")); got != CodeAddFailed {
		t.Fatalf("classifyAddErr(random) = %q, want %q", got, CodeAddFailed)
	}
}

// TestWriteFileSecret_AtomicReplace proves the atomic rotate path: writing over
// an existing key replaces it cleanly, keeps 0600, and leaves no staging temp
// file behind. The temp-file-then-rename design is what keeps a failed write
// from truncating the old key.
func TestWriteFileSecret_AtomicReplace(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := writeFileSecret("glm.key", []byte("old-key")); err != nil {
		t.Fatalf("writeFileSecret (create): %v", err)
	}

	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	path := filepath.Join(dir, "glm.key")

	// Rotate in place — must replace the old contents, not append to them.
	if err := writeFileSecret("glm.key", []byte("new-rotated-key")); err != nil {
		t.Fatalf("writeFileSecret (rotate): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if string(got) != "new-rotated-key" {
		t.Fatalf("secret = %q, want %q", string(got), "new-rotated-key")
	}

	// 0600 must survive the rename.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat secret: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("secret mode = %o, want 0600", perm)
		}
	}

	// No staging temp file should linger in the secrets dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read secrets dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("leftover staging temp file in secrets dir: %s", e.Name())
		}
	}
}

// TestVendorsConfigLock_ConcurrentEdits_NoLostUpdate: N concurrent Edit calls
// each mutate a DIFFERENT seeded vendor;
// because every Edit does a full config.Load → mutate → config.Save against the
// one global vendors.toml, without the global flock the last writer would
// clobber the others' rows (lost update). Under the lock every edit must
// survive. Runs clean under `go test -race`.
func TestVendorsConfigLock_ConcurrentEdits_NoLostUpdate(t *testing.T) {
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const n = 8
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("v%d", i)
		seedVendor(t, names[i]) // DefaultModel defaults to "<name>-flash"
	}

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			model := fmt.Sprintf("edited-%d", i)
			if _, err := Edit(EditRequest{Name: names[i], DefaultModel: &model}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Edit: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load post-edits: %v", err)
	}
	for i := 0; i < n; i++ {
		v, ok := cfg.Vendors[names[i]]
		if !ok {
			t.Fatalf("vendor %q lost from vendors.toml after concurrent edits", names[i])
		}
		want := fmt.Sprintf("edited-%d", i)
		if v.DefaultModel != want {
			t.Fatalf("vendor %q DefaultModel = %q, want %q (lost update — lock failed)",
				names[i], v.DefaultModel, want)
		}
	}
}

// chmodForTest sets perm on path and restores 0700 on cleanup. Used by the
// Remove-ordering failure-injection tests below.
func chmodForTest(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s %o: %v", path, perm, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

// TestRemove_ConfigSaveFailure_LeavesArtifactsIntact is the load-bearing case:
// when config.Save fails, vendors.toml must be UNCHANGED and the profile +
// secret must still exist — never a config row pointing at already-deleted
// artifacts (deleting profile+secret first would orphan the config row AND lose
// the key). We force the save to fail by making ConfigDir read-only after the
// lock file already exists.
func TestRemove_ConfigSaveFailure_LeavesArtifactsIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection needs a non-root euid")
	}
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v := seedVendor(t, "glm")
	secretPath := seedSecretFile(t, v.SecretRef, "sk-keep")
	profPath, err := profile.WriteForVendor(v, "/usr/bin/cc-fleet")
	if err != nil {
		t.Fatalf("WriteForVendor: %v", err)
	}

	// Acquire+release the vendors lock once so its lock file exists BEFORE we
	// lock the dir down (a no-field Edit is the cheapest such op; it Saves
	// successfully while the dir is still writable).
	if _, err := Edit(EditRequest{Name: "glm"}); err != nil {
		t.Fatalf("warm-up Edit: %v", err)
	}

	cfgDir, err := config.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	chmodForTest(t, cfgDir, 0o500) // read+exec, no write → AtomicWrite CreateTemp fails

	_, rmErr := Remove(RemoveRequest{Name: "glm"})
	if rmErr == nil {
		t.Fatal("Remove succeeded despite read-only ConfigDir; expected CONFIG_SAVE_FAILED")
	}
	var op *Op
	if !errors.As(rmErr, &op) || op.Code != CodeConfigSaveFailed {
		t.Fatalf("Remove err = %v, want CONFIG_SAVE_FAILED", rmErr)
	}

	// Restore write so we can inspect, then assert: config STILL references glm,
	// and the profile + secret are both intact (no destructive cleanup ran).
	if err := os.Chmod(cfgDir, 0o700); err != nil {
		t.Fatalf("restore ConfigDir perms: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load post-failed-remove: %v", err)
	}
	if _, ok := cfg.Vendors["glm"]; !ok {
		t.Fatal("vendor glm dropped from vendors.toml despite save failure (dangling-reference window)")
	}
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("secret deleted despite save failure: %v", err)
	}
	if _, err := os.Stat(profPath); err != nil {
		t.Fatalf("profile deleted despite save failure: %v", err)
	}
}

// TestRemove_ProfileDeleteFailure_RowAlreadyCommitted: when profile removal
// fails (profiles dir read-only), the config row is ALREADY committed gone, so
// there is no config row pointing at the (still-present) profile — the safe
// direction.
func TestRemove_ProfileDeleteFailure_RowAlreadyCommitted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection needs a non-root euid")
	}
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v := seedVendor(t, "glm")
	profPath, err := profile.WriteForVendor(v, "/usr/bin/cc-fleet")
	if err != nil {
		t.Fatalf("WriteForVendor: %v", err)
	}
	chmodForTest(t, filepath.Dir(profPath), 0o500) // can't unlink the profile

	_, rmErr := Remove(RemoveRequest{Name: "glm"})
	if rmErr == nil {
		t.Fatal("Remove succeeded despite read-only profiles dir; expected PROFILE_WRITE_FAILED")
	}
	var op *Op
	if !errors.As(rmErr, &op) || op.Code != CodeProfileWriteFailed {
		t.Fatalf("Remove err = %v, want PROFILE_WRITE_FAILED", rmErr)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Vendors["glm"]; ok {
		t.Fatal("config still references glm after committed save; row should be gone (no dangling reference)")
	}
}

// TestRemove_SecretDeleteFailure_RowAlreadyCommitted: when secret removal fails
// (secrets dir read-only), the config row is already committed gone — no config
// row references the orphaned secret. Never destroyed-key + dangling-row.
func TestRemove_SecretDeleteFailure_RowAlreadyCommitted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection needs a non-root euid")
	}
	setupHome(t)
	if _, err := Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	v := seedVendor(t, "glm")
	secretPath := seedSecretFile(t, v.SecretRef, "sk-orphan")
	if _, err := profile.WriteForVendor(v, "/usr/bin/cc-fleet"); err != nil {
		t.Fatalf("WriteForVendor: %v", err)
	}
	secretsDir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	chmodForTest(t, secretsDir, 0o500) // can't unlink the secret

	_, rmErr := Remove(RemoveRequest{Name: "glm"})
	if rmErr == nil {
		t.Fatal("Remove succeeded despite read-only secrets dir; expected SECRET_REMOVE_FAILED")
	}
	var op *Op
	if !errors.As(rmErr, &op) || op.Code != CodeSecretRemoveFailed {
		t.Fatalf("Remove err = %v, want SECRET_REMOVE_FAILED", rmErr)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := cfg.Vendors["glm"]; ok {
		t.Fatal("config still references glm after committed save; row should be gone (no dangling reference)")
	}
	// The orphan secret is still on disk (delete failed) but nothing references
	// it — the safe direction (no key destroyed while config still claims it).
	if _, err := os.Stat(secretPath); err != nil {
		t.Fatalf("secret unexpectedly gone: %v", err)
	}
}
