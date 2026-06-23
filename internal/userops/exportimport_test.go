package userops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

func seedConfig(t *testing.T, providers []*config.Provider, def string) {
	t.Helper()
	c := &config.Config{Version: config.SchemaVersion, Providers: map[string]*config.Provider{}, DefaultProvider: def}
	for _, p := range providers {
		c.Providers[p.Name] = p
	}
	if err := config.Save(c); err != nil {
		t.Fatalf("seed save: %v", err)
	}
}

func anthropicProvider(name string) *config.Provider {
	return &config.Provider{
		Name: name, BaseURL: "https://api." + name + ".example", DefaultModel: name + "-x",
		ModelsEndpoint: "https://api." + name + ".example/v1/models",
		SecretBackend:  "file", SecretRef: name + ".key", Enabled: true,
	}
}

func openaiProvider(name string) *config.Provider {
	return &config.Provider{
		Name: name, Protocol: config.ProtocolOpenAIChat, SecretBackend: "file", SecretRef: name + ".key",
		BaseURL: "http://127.0.0.1:17225/", UpstreamURL: "https://api.openai.com/v1",
		ModelsEndpoint: "https://api.openai.com/v1/models", DefaultModel: "gpt-4o", Enabled: true,
	}
}

func TestExportImport_RoundTrip(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek"), anthropicProvider("glm")}, "deepseek")

	ex, err := Export(ExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Written) != 2 {
		t.Fatalf("written = %v, want 2", ex.Written)
	}
	if !strings.Contains(string(ex.Bundle), "bundle_version") {
		t.Error("bundle is missing bundle_version")
	}

	// Fresh machine: wipe the roster.
	seedConfig(t, nil, "")
	im, err := Import(ImportRequest{Bundle: ex.Bundle})
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Added) != 2 || im.DefaultSet != "deepseek" {
		t.Fatalf("import result = %+v", im)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 || cfg.DefaultProvider != "deepseek" {
		t.Fatalf("roster = %d providers, default %q", len(cfg.Providers), cfg.DefaultProvider)
	}
	if cfg.Providers["glm"].ModelsEndpoint != "https://api.glm.example/v1/models" {
		t.Errorf("a portable field was lost: %q", cfg.Providers["glm"].ModelsEndpoint)
	}
}

// TestExportImport_RichFieldsRoundTrip: every portable field the docs promise travels —
// model roster, effort, default_permission, key_rotation, enabled=false — survives a full
// export → import, not just models_endpoint.
func TestExportImport_RichFieldsRoundTrip(t *testing.T) {
	setupHome(t)
	src := &config.Provider{
		Name: "kimi", BaseURL: "https://api.kimi.example", DefaultModel: "kimi-k2",
		ModelsEndpoint: "https://api.kimi.example/v1/models", SecretBackend: "file", SecretRef: "kimi.key",
		StrongModel: "kimi-k2-strong", FastModel: "kimi-k2-fast", Effort: "high",
		DefaultPermission: "acceptEdits", KeyRotation: "round_robin", Enabled: false,
	}
	seedConfig(t, []*config.Provider{src}, "")

	ex, err := Export(ExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	seedConfig(t, nil, "") // fresh target
	if _, err := Import(ImportRequest{Bundle: ex.Bundle}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Providers["kimi"]
	if got == nil {
		t.Fatal("kimi not imported")
	}
	for _, c := range []struct{ field, want, have string }{
		{"base_url", src.BaseURL, got.BaseURL},
		{"default_model", src.DefaultModel, got.DefaultModel},
		{"models_endpoint", src.ModelsEndpoint, got.ModelsEndpoint},
		{"strong_model", src.StrongModel, got.StrongModel},
		{"fast_model", src.FastModel, got.FastModel},
		{"effort", src.Effort, got.Effort},
		{"default_permission", src.DefaultPermission, got.DefaultPermission},
		{"key_rotation", src.KeyRotation, got.KeyRotation},
		{"secret_backend", src.SecretBackend, got.SecretBackend},
		{"secret_ref", src.SecretRef, got.SecretRef},
	} {
		if c.have != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.have, c.want)
		}
	}
	if got.Enabled {
		t.Error("enabled = true, want false")
	}
}

func TestExport_IsKeyless(t *testing.T) {
	home := setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "")
	// Plant a key on disk; export must not slurp it.
	secretsDir := filepath.Join(home, ".config", "cc-fleet", "secrets")
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "deepseek.key"), []byte("SK-SECRET-KEY-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	ex, err := Export(ExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ex.Bundle), "SK-SECRET-KEY-BYTES") {
		t.Fatal("bundle leaked key material")
	}
}

func TestExport_OmitsCodex(t *testing.T) {
	setupHome(t)
	codex := &config.Provider{
		Name: "codex", Protocol: config.ProtocolCodexOAuth, SecretBackend: "codex-oauth", SecretRef: "codex-oauth",
		BaseURL: "http://127.0.0.1:17299/", ModelsEndpoint: "http://127.0.0.1:17299/v1/models",
		DefaultModel: "gpt-5", Enabled: true,
	}
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek"), codex}, "")
	ex, err := Export(ExportRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Written) != 1 || ex.Written[0] != "deepseek" {
		t.Fatalf("written = %v, want [deepseek]", ex.Written)
	}
	if len(ex.OmittedCodex) != 1 || ex.OmittedCodex[0] != "codex" {
		t.Fatalf("omitted = %v, want [codex]", ex.OmittedCodex)
	}
	if strings.Contains(string(ex.Bundle), "[codex]") {
		t.Error("bundle still carries the codex row")
	}
}

func TestImport_StrictRefusalLeavesRosterUntouched(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "")

	// Unknown bundle_version.
	if _, err := Import(ImportRequest{Bundle: []byte("bundle_version = 99\n")}); err == nil {
		t.Error("unknown bundle_version should be rejected")
	}
	// A row that fails Config.Validate (non-http base_url).
	bad := "bundle_version = 1\n\n[bogus]\nbase_url = \"ftp://x\"\nmodels_endpoint = \"https://x/v1/models\"\nsecret_backend = \"file\"\nsecret_ref = \"x.key\"\n"
	if _, err := Import(ImportRequest{Bundle: []byte(bad)}); err == nil {
		t.Error("invalid provider row should be rejected")
	}
	cfg, _ := config.Load()
	if len(cfg.Providers) != 1 || cfg.Providers["bogus"] != nil {
		t.Fatalf("roster mutated on a rejected import: %d providers", len(cfg.Providers))
	}
}

func TestImport_CollisionSkipVsForce(t *testing.T) {
	setupHome(t)
	existing := anthropicProvider("deepseek")
	existing.DefaultModel = "old-model"
	seedConfig(t, []*config.Provider{existing}, "")

	// Bundle: a modified deepseek (collision) + a new glm.
	modified := anthropicProvider("deepseek")
	modified.DefaultModel = "new-model"
	bundle := mustBundle(t, []*config.Provider{modified, anthropicProvider("glm")}, "")

	// Without --force: glm added, deepseek skipped + unchanged.
	im, err := Import(ImportRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Added) != 1 || im.Added[0] != "glm" || len(im.SkippedCollision) != 1 {
		t.Fatalf("no-force result = %+v", im)
	}
	cfg, _ := config.Load()
	if cfg.Providers["deepseek"].DefaultModel != "old-model" {
		t.Error("collision was overwritten without --force")
	}

	// With --force, against a clean roster of just the original deepseek: deepseek
	// overwritten, glm added, backup written.
	seedConfig(t, []*config.Provider{existing}, "")
	im, err = Import(ImportRequest{Bundle: bundle, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Overwritten) != 1 || im.Overwritten[0] != "deepseek" || len(im.Added) != 1 || im.BackupPath == "" {
		t.Fatalf("force result = %+v", im)
	}
	if _, err := os.Stat(im.BackupPath); err != nil {
		t.Errorf("backup not written: %v", err)
	}
	cfg, _ = config.Load()
	if cfg.Providers["deepseek"].DefaultModel != "new-model" {
		t.Error("--force did not overwrite the colliding row")
	}
}

// TestImport_ForceBackupsAreDistinct: two rapid --force imports (same wall-clock second)
// must produce distinct backups, both preserved — the first backup is the only copy of the
// pre-import config and must never be clobbered.
func TestImport_ForceBackupsAreDistinct(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "")
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("deepseek")}, "")

	im1, err := Import(ImportRequest{Bundle: bundle, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	im2, err := Import(ImportRequest{Bundle: bundle, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if im1.BackupPath == "" || im2.BackupPath == "" || im1.BackupPath == im2.BackupPath {
		t.Fatalf("backups not distinct: %q vs %q", im1.BackupPath, im2.BackupPath)
	}
	for _, p := range []string{im1.BackupPath, im2.BackupPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("backup %s missing: %v", p, err)
		}
	}
}

func TestImport_OpenAIPortRederived(t *testing.T) {
	setupHome(t)
	seedConfig(t, nil, "")
	bundle := mustBundle(t, []*config.Provider{openaiProvider("oai")}, "")
	// The bundle drops the loopback base_url.
	if strings.Contains(string(bundle), "127.0.0.1") {
		t.Error("bundle should not carry the machine-local loopback base_url")
	}
	im, err := Import(ImportRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if len(im.Added) != 1 {
		t.Fatalf("import = %+v", im)
	}
	cfg, _ := config.Load()
	if got := cfg.Providers["oai"].BaseURL; !strings.HasPrefix(got, "http://127.0.0.1:") {
		t.Errorf("openai base_url not re-derived: %q", got)
	}
}

// TestImport_ForceDaemonPortsDistinct guards the batch allocator: a new daemon-backed
// provider whose name sorts BEFORE a forced daemon-backed overwrite must not take the
// port the overwrite reuses — the two must end on distinct loopback base_urls.
func TestImport_ForceDaemonPortsDistinct(t *testing.T) {
	setupHome(t)
	existing := openaiProvider("zoai")
	existing.BaseURL = "http://127.0.0.1:17222/"
	seedConfig(t, []*config.Provider{existing}, "")

	// "aoai" (new) sorts before "zoai" (forced overwrite); both daemon-backed.
	bundle := mustBundle(t, []*config.Provider{openaiProvider("aoai"), openaiProvider("zoai")}, "")
	if _, err := Import(ImportRequest{Bundle: bundle, Force: true}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load()
	if a, z := cfg.Providers["aoai"].BaseURL, cfg.Providers["zoai"].BaseURL; a == z {
		t.Fatalf("two daemon-backed providers share a loopback base_url: %q", a)
	}
}

// TestImport_RejectsVersionProviderRow: a provider named "version" collides with the
// always-written providers.toml `version` scalar; config.Validate must reject it so import
// can't persist an unparseable config. (Also closes the latent same bug in `cc-fleet add`.)
func TestImport_RejectsVersionProviderRow(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "")
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("version")}, "")
	if _, err := Import(ImportRequest{Bundle: bundle}); err == nil {
		t.Fatal("a [version] provider row should be rejected (clashes with the version scalar)")
	}
	if cfg, _ := config.Load(); cfg.Providers["version"] != nil || len(cfg.Providers) != 1 {
		t.Errorf("roster mutated on a rejected import: %d providers", len(cfg.Providers))
	}
}

func TestImport_DanglingDefaultDropped(t *testing.T) {
	setupHome(t)
	seedConfig(t, nil, "")
	// Bundle whose default names a provider not in the bundle.
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("glm")}, "deepseek")
	im, err := Import(ImportRequest{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if im.DefaultDropped != "deepseek" || im.DefaultSet != "" {
		t.Fatalf("dangling default not dropped: %+v", im)
	}
	cfg, _ := config.Load()
	if cfg.DefaultProvider != "" {
		t.Errorf("dangling default was written: %q", cfg.DefaultProvider)
	}
}

// TestImport_RejectsReservedNativeName: a hand-edited bundle with a [claude] row must be
// rejected (the name is reserved for the native leaf) and leave the roster untouched.
// TestExport_RejectsReservedProviderName: a provider whose name collides with a reserved
// bundle key (bundle_version) must fail export rather than emit an unparseable bundle.
func TestExport_RejectsReservedProviderName(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("bundle_version")}, "")
	if _, err := Export(ExportRequest{}); err == nil {
		t.Fatal("a provider named bundle_version should fail export (reserved-key clash)")
	}
}

func TestImport_RejectsReservedNativeName(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "")
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("claude")}, "")
	if _, err := Import(ImportRequest{Bundle: bundle, Force: true}); err == nil {
		t.Fatal("a bundle with the reserved [claude] row should be rejected")
	}
	cfg, _ := config.Load()
	if cfg.Providers["claude"] != nil || len(cfg.Providers) != 1 {
		t.Errorf("reserved row written or roster mutated: %d providers", len(cfg.Providers))
	}
}

// TestImport_DefaultNotRepinnedWithoutForce: a non-force import must not silently change
// an existing different default (mirrors SetDefaultProvider's force rule); --force does.
func TestImport_DefaultNotRepinnedWithoutForce(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("deepseek")}, "deepseek")
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("glm")}, "glm")

	im, err := Import(ImportRequest{Bundle: bundle}) // no force
	if err != nil {
		t.Fatal(err)
	}
	if im.DefaultSet != "" || im.DefaultDropped != "glm" {
		t.Fatalf("no-force default = %+v", im)
	}
	if cfg, _ := config.Load(); cfg.DefaultProvider != "deepseek" {
		t.Errorf("existing default silently changed to %q", cfg.DefaultProvider)
	}

	im, err = Import(ImportRequest{Bundle: bundle, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if im.DefaultSet != "glm" {
		t.Fatalf("force default = %+v", im)
	}
	if cfg, _ := config.Load(); cfg.DefaultProvider != "glm" {
		t.Errorf("--force did not change the default: %q", cfg.DefaultProvider)
	}
}

// TestImport_ReservedDefaultDroppedEvenIfRowExists: a bundle default of "claude" must be
// dropped even on a target that already holds a (hand-edited) [claude] row — the reserved
// leaf can never be the default.
func TestImport_ReservedDefaultDroppedEvenIfRowExists(t *testing.T) {
	setupHome(t)
	// A stale [claude] row that config.Load tolerates (Validate doesn't reject reserved names).
	seedConfig(t, []*config.Provider{anthropicProvider("claude"), anthropicProvider("glm")}, "")
	bundle := mustBundle(t, []*config.Provider{anthropicProvider("deepseek")}, "claude")
	im, err := Import(ImportRequest{Bundle: bundle, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if im.DefaultSet == "claude" || im.DefaultDropped != "claude" {
		t.Fatalf("reserved default not dropped: %+v", im)
	}
	if cfg, _ := config.Load(); cfg.DefaultProvider == config.ReservedNativeProvider {
		t.Error("default_provider was set to the reserved native leaf")
	}
}

func TestParseBundle_Rejects(t *testing.T) {
	if _, err := config.ParseBundle([]byte("version = 1\n")); err == nil {
		t.Error("a providers.toml (version, not bundle_version) should be rejected")
	}
	if _, err := config.ParseBundle([]byte("bundle_version = 2\n")); err == nil {
		t.Error("an unknown bundle_version should be rejected")
	}
	// A [default_provider] provider table (not the scalar) must be rejected, symmetric
	// with MarshalBundle, so import can't persist a bundle-incompatible row.
	dp := "bundle_version = 1\n\n[default_provider]\nbase_url = \"https://x\"\nmodels_endpoint = \"https://x/v1/models\"\nsecret_backend = \"file\"\nsecret_ref = \"x.key\"\n"
	if _, err := config.ParseBundle([]byte(dp)); err == nil {
		t.Error("a [default_provider] provider table should be rejected")
	}
}

// mustBundle builds a bundle directly via MarshalBundle (bypassing Export) so a test can
// craft arbitrary rows. It applies the same portability transform Export does.
func mustBundle(t *testing.T, providers []*config.Provider, def string) []byte {
	t.Helper()
	b := &config.Bundle{Version: config.BundleVersion, DefaultProvider: def, Providers: map[string]*config.Provider{}}
	for _, p := range providers {
		c := *p
		if c.DaemonBacked() {
			c.BaseURL = ""
		}
		b.Providers[p.Name] = &c
	}
	data, err := config.MarshalBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
