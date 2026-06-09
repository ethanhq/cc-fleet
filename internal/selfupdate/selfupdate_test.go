package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/version"
)

// --- LatestTag ---------------------------------------------------------------

func TestLatestTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+repo+"/releases/latest" {
			w.Header().Set("Location", "/"+repo+"/releases/tag/v9.9.9")
			w.WriteHeader(http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	withGitHubBase(t, srv.URL)

	tag, err := LatestTag(context.Background())
	if err != nil {
		t.Fatalf("LatestTag: %v", err)
	}
	if tag != "v9.9.9" {
		t.Fatalf("tag = %q, want v9.9.9", tag)
	}
}

func TestLatestTag_NoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A repo with no releases redirects /releases/latest to /releases.
		w.Header().Set("Location", "/"+repo+"/releases")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	withGitHubBase(t, srv.URL)

	if _, err := LatestTag(context.Background()); err == nil {
		t.Fatalf("LatestTag: want error when no release tag, got nil")
	}
}

// --- detectMethod ------------------------------------------------------------

func TestDetectMethod_Manifest(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	writeManifest(t, dir, `{"method":"tarball","plugin_scope":"project"}`)
	m, man := detectMethod(exe)
	if m != MethodTarball {
		t.Fatalf("method = %q, want tarball", m)
	}
	if man.PluginScope != "project" {
		t.Fatalf("plugin_scope = %q, want project", man.PluginScope)
	}
}

func TestDetectMethod_NpmManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"method":"npm"}`)
	if m, _ := detectMethod(filepath.Join(dir, "cc-fleet")); m != MethodNpm {
		t.Fatalf("method = %q, want npm", m)
	}
}

func TestDetectMethod_NpmHeuristic(t *testing.T) {
	// No manifest; a node_modules path falls back to the npm heuristic.
	exe := filepath.Join(t.TempDir(), "node_modules", "@ethanhq", "cc-fleet", "bin", "cc-fleet")
	if m, _ := detectMethod(exe); m != MethodNpm {
		t.Fatalf("method = %q, want npm (node_modules heuristic)", m)
	}
}

func TestDetectMethod_GoHeuristic(t *testing.T) {
	gobin := t.TempDir()
	t.Setenv("GOBIN", gobin)
	if m, _ := detectMethod(filepath.Join(gobin, "cc-fleet")); m != MethodGo {
		t.Fatalf("method = %q, want go (GOBIN heuristic)", m)
	}
}

func TestDetectMethod_Unknown(t *testing.T) {
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	t.Setenv("HOME", t.TempDir())
	if m, _ := detectMethod(filepath.Join(t.TempDir(), "cc-fleet")); m != MethodUnknown {
		t.Fatalf("method = %q, want unknown", m)
	}
}

// --- checksumFor -------------------------------------------------------------

func TestChecksumFor(t *testing.T) {
	sums := "abc123  cc-fleet-linux-amd64.tar.gz\ndef456  cc-fleet-darwin-arm64.tar.gz\n"
	if got := checksumFor(sums, "cc-fleet-darwin-arm64.tar.gz"); got != "def456" {
		t.Fatalf("checksumFor = %q, want def456", got)
	}
	if got := checksumFor(sums, "missing.tar.gz"); got != "" {
		t.Fatalf("checksumFor(missing) = %q, want empty", got)
	}
}

// --- smokeTest ---------------------------------------------------------------

func TestSmokeTest(t *testing.T) {
	bin := writeScript(t, t.TempDir(), "cc-fleet", `echo "cc-fleet version v9.9.9"`)
	if err := smokeTest(context.Background(), bin, "v9.9.9"); err != nil {
		t.Fatalf("smokeTest: %v", err)
	}
	// Wrong reported version → fail.
	bad := writeScript(t, t.TempDir(), "cc-fleet", `echo "cc-fleet version v0.0.1"`)
	if err := smokeTest(context.Background(), bad, "v9.9.9"); err == nil {
		t.Fatalf("smokeTest: want error on version mismatch")
	}
}

// --- swapBinary + Rollback ---------------------------------------------------

func TestSwapBinaryAndRollback(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	if err := os.WriteFile(exe, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(dir, ".cc-fleet-update-staged")
	if err := os.WriteFile(staged, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBinary(exe, staged, "v0.1.0", "v9.9.9", &bytes.Buffer{}); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "NEW" {
		t.Fatalf("after swap exe = %q, want NEW", b)
	}
	if b, _ := os.ReadFile(exe + ".previous"); string(b) != "OLD" {
		t.Fatalf(".previous = %q, want OLD", b)
	}
	// Rollback restores OLD.
	withExe(t, exe)
	if err := Rollback(&bytes.Buffer{}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatalf("after rollback exe = %q, want OLD", b)
	}
}

func TestRollback_NoBackup(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	_ = os.WriteFile(exe, []byte("X"), 0o755)
	withExe(t, exe)
	if err := Rollback(&bytes.Buffer{}); err == nil {
		t.Fatalf("Rollback: want error when no .previous backup")
	}
}

// --- Run (end-to-end tarball self-update) ------------------------------------

func TestRun_TarballEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	withVersion(t, "v0.1.0")

	bindir := filepath.Join(home, "bin")
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := writeScript(t, bindir, "cc-fleet", `echo "cc-fleet version v0.1.0"`)
	writeManifest(t, bindir, `{"method":"tarball"}`)
	withExe(t, exe)

	// Serve a release whose binary prints v9.9.9.
	tarName := fmt.Sprintf("cc-fleet-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	tarGz := buildReleaseTarGz(t, "#!/bin/sh\necho \"cc-fleet version v9.9.9\"\n")
	sums := fmt.Sprintf("%s  %s\n", sha256Hex(tarGz), tarName)
	mux := http.NewServeMux()
	mux.HandleFunc("/"+repo+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/"+repo+"/releases/tag/v9.9.9")
		w.WriteHeader(http.StatusFound)
	})
	mux.HandleFunc("/"+repo+"/releases/download/v9.9.9/"+tarName, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarGz)
	})
	mux.HandleFunc("/"+repo+"/releases/download/v9.9.9/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sums))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	withGitHubBase(t, srv.URL)

	var out bytes.Buffer
	// BinaryOnly: skip the plugin step (no real `claude` in the test env).
	if err := Run(context.Background(), Options{BinaryOnly: true, Out: &out}); err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Contains(got, []byte("v9.9.9")) {
		t.Fatalf("after update exe is not the new binary:\n%s", got)
	}
	if _, err := os.Stat(exe + ".previous"); err != nil {
		t.Fatalf(".previous backup missing: %v", err)
	}
}

func TestRun_DevBuildIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	withVersion(t, "0.1.0-dev") // non-release → not comparable

	bindir := filepath.Join(home, "bin")
	_ = os.MkdirAll(bindir, 0o755)
	exe := writeScript(t, bindir, "cc-fleet", `echo "cc-fleet version 0.1.0-dev"`)
	withExe(t, exe)
	// Reaching the network would be a bug; serve nothing reachable.
	withGitHubBase(t, "http://127.0.0.1:0")

	var out bytes.Buffer
	if err := Run(context.Background(), Options{BinaryOnly: true, Out: &out}); err != nil {
		t.Fatalf("Run dev build: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("Development build")) {
		t.Fatalf("dev build output = %q, want a 'Development build' notice", out.String())
	}
}

// --- cache / startup prompt --------------------------------------------------

func TestPromptTag_FreshNewer(t *testing.T) {
	setHome(t)
	withVersion(t, "v0.1.0")
	writeCache(t, `{"latest_tag":"v0.1.8"}`)
	tag, ok := PromptTag(time.Unix(1_000_000, 0))
	if !ok || tag != "v0.1.8" {
		t.Fatalf("PromptTag = (%q,%v), want (v0.1.8,true)", tag, ok)
	}
}

func TestPromptTag_NotNewer(t *testing.T) {
	setHome(t)
	withVersion(t, "v0.1.8")
	writeCache(t, `{"latest_tag":"v0.1.8"}`)
	if _, ok := PromptTag(time.Unix(1_000_000, 0)); ok {
		t.Fatalf("PromptTag ok=true, want false when latest == current")
	}
}

func TestPromptTag_DevBuildNeverPrompts(t *testing.T) {
	setHome(t)
	withVersion(t, "0.1.0-dev")
	writeCache(t, `{"latest_tag":"v9.9.9"}`)
	if _, ok := PromptTag(time.Unix(1_000_000, 0)); ok {
		t.Fatalf("PromptTag ok=true for a dev build, want false")
	}
}

func TestPromptTag_OptedOut(t *testing.T) {
	setHome(t)
	withVersion(t, "v0.1.0")
	writeCache(t, `{"latest_tag":"v0.1.8"}`)
	t.Setenv(OptOutEnv, "1")
	if _, ok := PromptTag(time.Unix(1_000_000, 0)); ok {
		t.Fatalf("PromptTag ok=true while opted out, want false")
	}
}

func TestPromptTag_DismissedWindow(t *testing.T) {
	setHome(t)
	withVersion(t, "v0.1.0")
	now := time.Unix(1_000_000, 0)
	// Dismissed just now → suppressed.
	writeCache(t, `{"latest_tag":"v0.1.8","dismissed_tag":"v0.1.8","dismissed_at":1000000}`)
	if _, ok := PromptTag(now); ok {
		t.Fatalf("PromptTag ok=true right after dismissal, want false")
	}
	// Dismissed > 24h ago → prompts again.
	later := now.Add(25 * time.Hour)
	if _, ok := PromptTag(later); !ok {
		t.Fatalf("PromptTag ok=false 25h after dismissal, want true")
	}
}

func TestDismissSuppresses(t *testing.T) {
	setHome(t)
	withVersion(t, "v0.1.0")
	writeCache(t, `{"latest_tag":"v0.1.8"}`)
	now := time.Unix(1_000_000, 0)
	if _, ok := PromptTag(now); !ok {
		t.Fatalf("precondition: PromptTag should be true before dismissal")
	}
	if err := Dismiss("v0.1.8", now); err != nil {
		t.Fatalf("Dismiss: %v", err)
	}
	if _, ok := PromptTag(now); ok {
		t.Fatalf("PromptTag ok=true after Dismiss within the window, want false")
	}
}

func TestRefreshCache(t *testing.T) {
	setHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "/"+repo+"/releases/tag/v9.9.9")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	withGitHubBase(t, srv.URL)

	now := time.Unix(2_000_000, 0)
	RefreshCache(context.Background(), now) // empty cache → fetches
	c := loadCache()
	if c.LatestTag != "v9.9.9" || c.LastChecked != now.Unix() {
		t.Fatalf("after refresh cache = %+v, want latest v9.9.9 @ %d", c, now.Unix())
	}

	// A fresh cache (checked just now) is not re-queried even if the server moves.
	RefreshCache(context.Background(), now.Add(time.Hour))
	if c2 := loadCache(); c2.LatestTag != "v9.9.9" || c2.LastChecked != now.Unix() {
		t.Fatalf("fresh cache was re-queried: %+v", c2)
	}
}

// --- helpers -----------------------------------------------------------------

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func writeCache(t *testing.T, body string) {
	t.Helper()
	p, err := cachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func withGitHubBase(t *testing.T, base string) {
	t.Helper()
	orig := githubBase
	t.Cleanup(func() { githubBase = orig })
	githubBase = base
}

func withExe(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	t.Cleanup(func() { osExecutable = orig })
	osExecutable = func() (string, error) { return path, nil }
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = v
}

func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, manifestName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func buildReleaseTarGz(t *testing.T, script string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(script)
	hdr := &tar.Header{
		Name:     fmt.Sprintf("cc-fleet-%s-%s/cc-fleet", runtime.GOOS, runtime.GOARCH),
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
