package selfupdate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubExe points exePath at a fake binary for the duration of the test.
func stubExe(t *testing.T, path string) {
	t.Helper()
	old := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = old })
}

// stubRunner replaces execRunner with a recorder that never executes anything.
func stubRunner(t *testing.T, fail bool) *[][]string {
	t.Helper()
	var calls [][]string
	old := execRunner
	execRunner = func(_ context.Context, _ io.Writer, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if fail {
			return os.ErrPermission
		}
		return nil
	}
	t.Cleanup(func() { execRunner = old })
	return &calls
}

// tarballLayout plants a full tarball-install dir: binary, ccf symlink,
// manifest, rollback backup. Returns the exe path and all four paths.
func tarballLayout(t *testing.T, method string) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	alias := filepath.Join(dir, "ccf")
	if err := os.Symlink("cc-fleet", alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	man := filepath.Join(dir, manifestName)
	data, _ := json.Marshal(map[string]string{"method": method})
	if err := os.WriteFile(man, data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	prev := exe + ".previous"
	if err := os.WriteFile(prev, []byte("old"), 0o755); err != nil {
		t.Fatalf("write previous: %v", err)
	}
	return exe, []string{exe, alias, man, prev}
}

func TestUninstallBinary_TarballRemovesArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix removal path (symlink alias); windows returns manual commands")
	}
	exe, paths := tarballLayout(t, "tarball")
	// Run "via the ccf alias" so the symlink-resolution path is exercised too.
	stubExe(t, filepath.Join(filepath.Dir(exe), "ccf"))
	calls := stubRunner(t, false)

	removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
	if len(kept) != 0 || len(manual) != 0 {
		t.Fatalf("kept=%v manual=%v, want none", kept, manual)
	}
	if len(*calls) != 0 {
		t.Fatalf("tarball removal must not shell out, ran %v", *calls)
	}
	if len(removed) != len(paths) {
		t.Fatalf("removed=%v, want all of %v", removed, paths)
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s still exists", p)
		}
	}
}

func TestUninstallBinary_UnknownOnlyPrints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix branch; windows always returns manual commands")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	// No manifest; make sure no go-bin heuristic can match the temp dir.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GOBIN", "")
	t.Setenv("GOPATH", "")
	stubExe(t, exe)

	removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
	if len(removed) != 0 || len(kept) != 0 {
		t.Fatalf("removed=%v kept=%v, want none (unknown method must not delete)", removed, kept)
	}
	if len(manual) == 0 || !strings.Contains(manual[0], exe) {
		t.Fatalf("manual=%v, want an rm command for %s", manual, exe)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("exe deleted under unknown method: %v", err)
	}
}

func TestUninstallBinary_Npm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix branch; windows always returns manual commands")
	}

	t.Run("npm missing", func(t *testing.T) {
		exe, _ := tarballLayout(t, "npm")
		stubExe(t, exe)
		t.Setenv("PATH", t.TempDir())
		calls := stubRunner(t, false)
		removed, _, manual := UninstallBinary(context.Background(), io.Discard)
		if len(removed) != 0 || len(*calls) != 0 {
			t.Fatalf("removed=%v calls=%v, want the manual command only", removed, *calls)
		}
		if len(manual) != 1 || !strings.Contains(manual[0], npmPackage) {
			t.Fatalf("manual=%v, want the npm uninstall command", manual)
		}
	})

	t.Run("npm runs", func(t *testing.T) {
		exe, paths := tarballLayout(t, "npm")
		stubExe(t, exe)
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake npm: %v", err)
		}
		t.Setenv("PATH", bin)
		// Record the call and remove the tree the way a real prefix-matching
		// `npm uninstall -g` would.
		var calls [][]string
		old := execRunner
		execRunner = func(_ context.Context, _ io.Writer, name string, args ...string) error {
			calls = append(calls, append([]string{name}, args...))
			for _, p := range paths {
				_ = os.Remove(p)
			}
			return nil
		}
		t.Cleanup(func() { execRunner = old })

		removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
		if len(manual) != 0 || len(kept) != 0 {
			t.Fatalf("manual=%v kept=%v, want none when npm removes the tree", manual, kept)
		}
		if len(removed) != 1 || !strings.Contains(removed[0], npmPackage) {
			t.Fatalf("removed=%v, want the npm package entry", removed)
		}
		want := []string{"npm", "uninstall", "-g", npmPackage}
		if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(want, " ") {
			t.Fatalf("calls=%v, want %v", calls, want)
		}
	})

	t.Run("wrong npm prefix", func(t *testing.T) {
		exe, _ := tarballLayout(t, "npm")
		stubExe(t, exe)
		bin := t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake npm: %v", err)
		}
		t.Setenv("PATH", bin)
		stubRunner(t, false) // "succeeds" but the exe survives — wrong prefix

		removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
		if len(removed) != 0 {
			t.Fatalf("removed=%v, want none when the exe survives npm uninstall", removed)
		}
		if len(kept) == 0 || !strings.Contains(kept[0], "different prefix") {
			t.Fatalf("kept=%v, want the wrong-prefix note", kept)
		}
		if len(manual) != 1 || !strings.Contains(manual[0], npmPackage) {
			t.Fatalf("manual=%v, want the npm command back", manual)
		}
	})
}

func TestUninstallBinary_UnrecognizedMethodOnlyPrints(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix branch; windows always returns manual commands")
	}
	exe, paths := tarballLayout(t, "homebrew") // a method this binary doesn't know
	stubExe(t, exe)
	calls := stubRunner(t, false)

	removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
	if len(removed) != 0 || len(kept) != 0 || len(*calls) != 0 {
		t.Fatalf("removed=%v kept=%v calls=%v, want manual-only for an unrecognized method", removed, kept, *calls)
	}
	if len(manual) == 0 {
		t.Fatal("manual empty, want removal commands")
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s deleted under an unrecognized method", p)
		}
	}
}

func TestUninstallBinary_WindowsManualOnly(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only branch")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet.exe")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	data, _ := json.Marshal(map[string]string{"method": "tarball"})
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	stubExe(t, exe)

	removed, kept, manual := UninstallBinary(context.Background(), io.Discard)
	if len(removed) != 0 || len(kept) != 0 {
		t.Fatalf("removed=%v kept=%v, want none on windows", removed, kept)
	}
	if joined := strings.Join(manual, "\n"); !strings.Contains(joined, "after this process exits") || !strings.Contains(joined, "del ") {
		t.Fatalf("manual=%v, want the run-after note + del commands", manual)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatalf("exe deleted on windows: %v", err)
	}
}

// TestBinaryArtifactsFor_WindowsListsSiblingCopy: the windows installers copy
// the exe to both canonical names, so the artifact list carries the sibling
// copy — through either invocation name — plus the manifest; a sibling that
// does not exist is simply absent.
func TestBinaryArtifactsFor_WindowsListsSiblingCopy(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"cc-fleet.exe", "ccf.exe", manifestName} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for exeName, sibling := range map[string]string{"cc-fleet.exe": "ccf.exe", "ccf.exe": "cc-fleet.exe"} {
		arts := binaryArtifactsFor(filepath.Join(dir, exeName), "windows")
		joined := strings.Join(arts, "\n")
		for _, want := range []string{filepath.Join(dir, sibling), filepath.Join(dir, manifestName), filepath.Join(dir, exeName)} {
			if !strings.Contains(joined, want) {
				t.Fatalf("artifacts for %s missing %s: %v", exeName, want, arts)
			}
		}
	}
	solo := t.TempDir()
	exe := filepath.Join(solo, "cc-fleet.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	for _, a := range binaryArtifactsFor(exe, "windows") {
		if strings.HasSuffix(a, "ccf.exe") {
			t.Fatalf("nonexistent sibling listed: %v", a)
		}
	}
}

// pluginSandbox points HOME at a temp dir, optionally planting a cached
// plugin. Returns the cache dir (so a test's runner stub can clear it the way
// a real `claude plugin uninstall` would).
func pluginSandbox(t *testing.T, cached bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".claude", "plugins", "cache", marketplace, "cc-fleet", "9.9.9")
	if cached {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir plugin cache: %v", err)
		}
	}
	return dir
}

func TestUninstallPlugin_NotCachedIsNoop(t *testing.T) {
	pluginSandbox(t, false)
	calls := stubRunner(t, false)
	removed, manual := UninstallPlugin(context.Background(), io.Discard)
	if removed != nil || manual != nil || len(*calls) != 0 {
		t.Fatalf("removed=%v manual=%v calls=%v, want a no-op", removed, manual, *calls)
	}
}

func TestUninstallPlugin_ManualWhenClaudeMissing(t *testing.T) {
	pluginSandbox(t, true)
	t.Setenv("PATH", t.TempDir())
	removed, manual := UninstallPlugin(context.Background(), io.Discard)
	if removed != nil {
		t.Fatalf("removed=%v, want none without claude", removed)
	}
	if len(manual) != 2 || !strings.Contains(manual[0], pluginRef) || !strings.Contains(manual[1], marketplace) {
		t.Fatalf("manual=%v, want the plugin + marketplace commands", manual)
	}
	for _, c := range manual {
		if !strings.Contains(c, "--scope") {
			t.Fatalf("manual command %q lacks --scope (unscoped marketplace remove hits every scope)", c)
		}
	}
}

func TestUninstallPlugin_RunsClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	cache := pluginSandbox(t, true)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", bin)
	// Record calls AND clear the cache on the uninstall call, the way a real
	// `claude plugin uninstall` for the right scope would.
	var calls [][]string
	old := execRunner
	execRunner = func(_ context.Context, _ io.Writer, name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 1 && args[1] == "uninstall" {
			_ = os.RemoveAll(cache)
		}
		return nil
	}
	t.Cleanup(func() { execRunner = old })

	removed, manual := UninstallPlugin(context.Background(), io.Discard)
	if len(manual) != 0 {
		t.Fatalf("manual=%v, want none when claude succeeds and the cache clears", manual)
	}
	if len(removed) != 2 {
		t.Fatalf("removed=%v, want plugin + marketplace entries", removed)
	}
	if len(calls) != 2 || calls[0][1] != "plugin" || calls[1][3] != "remove" {
		t.Fatalf("calls=%v, want plugin uninstall then marketplace remove", calls)
	}
	// No manifest next to the test binary → the installers' default scope, on
	// BOTH calls (an unscoped marketplace remove would hit every scope).
	for i := range calls {
		if got := strings.Join(calls[i], " "); !strings.HasSuffix(got, "--scope user") {
			t.Fatalf("call %q, want a trailing --scope user", got)
		}
	}
}

// A guessed (manifest-less) user-scope attempt that leaves the cache behind
// means a project/local registration likely remains — the result must carry
// scoped manual commands instead of reporting a false complete.
func TestUninstallPlugin_GuessedScopeLeavesManualWhenCacheRemains(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	pluginSandbox(t, true)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", bin)
	stubRunner(t, false) // succeeds but never clears the cache

	_, manual := UninstallPlugin(context.Background(), io.Discard)
	if len(manual) != 2 {
		t.Fatalf("manual=%v, want scoped plugin + marketplace commands", manual)
	}
	for _, c := range manual {
		if !strings.Contains(c, "<user|project|local>") {
			t.Fatalf("manual command %q, want the scope-choice template", c)
		}
	}
}

func TestUninstallPlugin_UsesManifestScope(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	pluginSandbox(t, true)
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	data, _ := json.Marshal(map[string]string{"method": "tarball", "plugin_scope": "project"})
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	stubExe(t, exe)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", bin)
	calls := stubRunner(t, false)

	if _, manual := UninstallPlugin(context.Background(), io.Discard); len(manual) != 0 {
		t.Fatalf("manual=%v, want none", manual)
	}
	if got := strings.Join((*calls)[0], " "); !strings.HasSuffix(got, "--scope project") {
		t.Fatalf("uninstall call %q, want the manifest's project scope", got)
	}
}

func TestUninstallPlugin_ManifestPluginWithoutCacheStillUninstalls(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("plants a #!/bin/sh fake claude not runnable on windows")
	}
	pluginSandbox(t, false) // cache cleared — but the manifest recorded a plugin install
	dir := t.TempDir()
	exe := filepath.Join(dir, "cc-fleet")
	if err := os.WriteFile(exe, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	data, _ := json.Marshal(map[string]string{"method": "tarball", "plugin_scope": "local", "skill": "plugin"})
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	stubExe(t, exe)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", bin)
	calls := stubRunner(t, false)

	removed, _ := UninstallPlugin(context.Background(), io.Discard)
	if len(*calls) != 2 || len(removed) != 2 {
		t.Fatalf("calls=%v removed=%v, want the scoped uninstall to run despite the empty cache", *calls, removed)
	}
	if got := strings.Join((*calls)[0], " "); !strings.HasSuffix(got, "--scope local") {
		t.Fatalf("uninstall call %q, want the manifest's local scope", got)
	}
}
