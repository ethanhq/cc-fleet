package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleCache mirrors what a real refresh against deepseek + kimi would
// produce.
func sampleCache(now time.Time) *Cache {
	return &Cache{
		Version: CacheVersion,
		Vendors: map[string]*VendorCache{
			"deepseek": {
				Vendor:    "deepseek",
				Endpoint:  "https://api.deepseek.com/v1/models",
				FetchedAt: now,
				Models: []Model{
					{ID: "deepseek-v4-flash", OwnedBy: "deepseek"},
					{ID: "deepseek-v4-pro", OwnedBy: "deepseek"},
				},
			},
			"kimi": {
				Vendor:    "kimi",
				Endpoint:  "https://api.moonshot.cn/anthropic/v1/models",
				FetchedAt: now,
				Models: []Model{
					{ID: "kimi-latest"},
				},
			},
		},
	}
}

// isolateConfigDir points XDG_CONFIG_HOME at a fresh temp dir so Path() is
// sandboxed for the test. Returns the cc-fleet config root.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", filepath.Join(xdg, "fakehome"))
	return filepath.Join(xdg, "cc-fleet")
}

func TestPath_UsesConfigDir(t *testing.T) {
	root := isolateConfigDir(t)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(root, "models-cache.json")
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	isolateConfigDir(t)
	now := time.Date(2026, 5, 24, 8, 0, 0, 0, time.UTC)
	c := sampleCache(now)

	if err := Save(c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Fatalf("round-trip mismatch:\n got: %+v\nwant: %+v", got, c)
	}
}

func TestLoad_MissingFile_ReturnsEmpty(t *testing.T) {
	isolateConfigDir(t)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load(missing) = err %v, want nil", err)
	}
	if got == nil {
		t.Fatalf("Load(missing): want non-nil Cache, got nil")
	}
	if got.Version != CacheVersion {
		t.Fatalf("Version = %d, want %d", got.Version, CacheVersion)
	}
	if got.Vendors == nil {
		t.Fatalf("Vendors map is nil; want empty initialised map")
	}
	if len(got.Vendors) != 0 {
		t.Fatalf("Vendors len = %d, want 0", len(got.Vendors))
	}
}

func TestLoadFromPath_MissingFile_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	got, err := LoadFromPath(missing)
	if err != nil {
		t.Fatalf("LoadFromPath(missing) = err %v, want nil", err)
	}
	if got == nil || got.Vendors == nil {
		t.Fatalf("got = %+v, want non-nil Cache with non-nil Vendors", got)
	}
}

func TestSave_FilePerm0600(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(sampleCache(time.Now())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 0600", got)
	}
}

func TestSave_Atomic_NoTempLeft(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(sampleCache(time.Now())); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leaked temp file: %s", e.Name())
		}
	}
}

func TestSave_RejectsNil(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(nil); err == nil {
		t.Fatalf("Save(nil): want error, got nil")
	}
}

func TestSave_OverwriteExisting(t *testing.T) {
	isolateConfigDir(t)
	c1 := sampleCache(time.Date(2026, 5, 24, 0, 0, 0, 0, time.UTC))
	if err := Save(c1); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	c2 := sampleCache(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC))
	c2.Vendors["glm"] = &VendorCache{
		Vendor:    "glm",
		Endpoint:  "https://open.bigmodel.cn/api/anthropic/v1/models",
		FetchedAt: time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		Models:    []Model{{ID: "glm-4.6"}},
	}
	if err := Save(c2); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := got.Vendors["glm"]; !ok {
		t.Fatalf("Vendors missing glm after overwrite; got keys %v", vendorKeys(got))
	}
	if len(got.Vendors) != 3 {
		t.Fatalf("Vendors len = %d, want 3", len(got.Vendors))
	}
}

func TestSave_JSONShape(t *testing.T) {
	isolateConfigDir(t)
	now := time.Date(2026, 5, 24, 8, 0, 0, 0, time.UTC)
	if err := Save(sampleCache(now)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"version", "vendors"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("on-disk JSON missing %q field; got %v", key, rawKeys(raw))
		}
	}
}

func TestIsStale(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		vc   *VendorCache
		want bool
	}{
		{"nil", nil, true},
		{"zero fetched_at", &VendorCache{}, true},
		{"fresh (just now)", &VendorCache{FetchedAt: now}, false},
		{"fresh (1d ago)", &VendorCache{FetchedAt: now.Add(-24 * time.Hour)}, false},
		// Edge: exactly StaleAfter old is considered stale (>=).
		{"exact boundary", &VendorCache{FetchedAt: now.Add(-StaleAfter)}, true},
		// 1s younger than the boundary still counts as fresh.
		{"just inside window", &VendorCache{FetchedAt: now.Add(-StaleAfter + time.Second)}, false},
		// 1s past the boundary is stale.
		{"just past boundary", &VendorCache{FetchedAt: now.Add(-StaleAfter - time.Second)}, true},
		{"very old", &VendorCache{FetchedAt: now.Add(-365 * 24 * time.Hour)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsStale(tc.vc); got != tc.want {
				t.Fatalf("IsStale = %v, want %v", got, tc.want)
			}
		})
	}
}

func vendorKeys(c *Cache) []string {
	out := make([]string, 0, len(c.Vendors))
	for k := range c.Vendors {
		out = append(out, k)
	}
	return out
}

func rawKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
