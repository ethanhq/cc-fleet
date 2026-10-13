package claudepaths

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// setHome points the platform's home variable at dir (HOME on unix,
// USERPROFILE on Windows; both are set so the test is platform-neutral).
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestRootHonorsClaudeConfigDir(t *testing.T) {
	setHome(t, t.TempDir())
	cfg := filepath.Join(t.TempDir(), "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	if got := Root(); got != cfg {
		t.Fatalf("Root() = %q, want $CLAUDE_CONFIG_DIR %q", got, cfg)
	}
	for name, got := range map[string]string{
		"settings.json": Settings(),
		"agents":        Agents(),
		"sessions":      Sessions(),
		"teams":         Teams(),
	} {
		if want := filepath.Join(cfg, name); got != want {
			t.Errorf("path for %s = %q, want %q", name, got, want)
		}
	}

	// Verbatim: no ~ expansion, no absolutizing.
	for _, raw := range []string{"~/elsewhere", "relative/dir"} {
		t.Setenv("CLAUDE_CONFIG_DIR", raw)
		if got := Root(); got != raw {
			t.Errorf("Root() with CLAUDE_CONFIG_DIR=%q = %q, want it verbatim", raw, got)
		}
	}
}

func TestRootDefaultsToHome(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "") // empty counts as unset

	root := filepath.Join(home, ".claude")
	if got := Root(); got != root {
		t.Fatalf("Root() = %q, want %q", got, root)
	}
	for name, got := range map[string]string{
		"settings.json": Settings(),
		"agents":        Agents(),
		"sessions":      Sessions(),
		"teams":         Teams(),
	} {
		if want := filepath.Join(root, name); got != want {
			t.Errorf("path for %s = %q, want %q", name, got, want)
		}
	}

	// Not cached: a later env change is seen by the next call.
	other := t.TempDir()
	setHome(t, other)
	if got, want := Root(), filepath.Join(other, ".claude"); got != want {
		t.Fatalf("Root() after HOME change = %q, want %q (must not cache)", got, want)
	}
}

func TestRootEmptyWhenHomeUnresolvable(t *testing.T) {
	setHome(t, "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	for name, got := range map[string]string{
		"Root":         Root(),
		"Settings":     Settings(),
		"Agents":       Agents(),
		"Sessions":     Sessions(),
		"Teams":        Teams(),
		"GlobalConfig": GlobalConfig(),
	} {
		if got != "" {
			t.Errorf("%s() = %q with no resolvable home, want \"\"", name, got)
		}
	}
	if got := PluginVersionDirs("cc-fleet"); got != nil {
		t.Errorf("PluginVersionDirs() = %v with no resolvable home, want nil", got)
	}
}

// TestPluginVersionDirs: every <marketplace>/<plugin>/<version> under the config
// root is listed, and a root whose name holds glob metacharacters is read
// literally — "claude[work]" as a pattern would match the sibling "claudew".
func TestPluginVersionDirs(t *testing.T) {
	setHome(t, t.TempDir())
	base := t.TempDir()
	cfg := filepath.Join(base, "claude[work]")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	mk := func(root string, parts ...string) string {
		t.Helper()
		dir := filepath.Join(append([]string{root, "plugins", "cache"}, parts...)...)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	mk(filepath.Join(base, "claudew"), "ethanhq", "cc-fleet", "9.9.9")
	if got := PluginVersionDirs("cc-fleet"); got != nil {
		t.Fatalf("PluginVersionDirs() = %v, want nil: only the sibling directory has the plugin", got)
	}

	want := []string{
		mk(cfg, "ethanhq", "cc-fleet", "0.3.4"),
		mk(cfg, "ethanhq", "cc-fleet", "0.4.0"),
		mk(cfg, "other", "cc-fleet", "0.1.0"),
	}
	mk(cfg, "ethanhq", "another-plugin", "1.0.0")
	if got := PluginVersionDirs("cc-fleet"); !reflect.DeepEqual(got, want) {
		t.Fatalf("PluginVersionDirs() = %v, want %v", got, want)
	}
}

func TestGlobalConfigPlacement(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)

	// Default: a sibling of ~/.claude, not inside it.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, want := GlobalConfig(), filepath.Join(home, ".claude.json"); got != want {
		t.Fatalf("GlobalConfig() = %q, want %q", got, want)
	}

	// CLAUDE_CONFIG_DIR moves it inside the config dir.
	cfg := filepath.Join(t.TempDir(), "claude-config")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if got, want := GlobalConfig(), filepath.Join(cfg, ".claude.json"); got != want {
		t.Fatalf("GlobalConfig() with CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}

	// CLAUDE_CONFIG_DIR alone is enough: no home needed.
	setHome(t, "")
	if got, want := GlobalConfig(), filepath.Join(cfg, ".claude.json"); got != want {
		t.Fatalf("GlobalConfig() with CLAUDE_CONFIG_DIR and no home = %q, want %q", got, want)
	}
}

// TestPluginVersionDirs_SkipsOrphaned: a copy Claude Code marked with
// .orphaned_at (uninstalled, or replaced by an update) is not installed; the
// copy counts again once the marker is gone, as after a reinstall.
func TestPluginVersionDirs_SkipsOrphaned(t *testing.T) {
	setHome(t, t.TempDir())
	cfg := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plugin := filepath.Join(cfg, "plugins", "cache", "ethanhq", "cc-fleet")
	old, cur := filepath.Join(plugin, "0.3.4"), filepath.Join(plugin, "0.4.0")
	for _, dir := range []string{old, cur} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(old, ".orphaned_at")
	if err := os.WriteFile(marker, []byte("1791889510666"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := PluginVersionDirs("cc-fleet"), []string{cur}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PluginVersionDirs() = %v, want %v", got, want)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if got, want := PluginVersionDirs("cc-fleet"), []string{old, cur}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PluginVersionDirs() = %v, want %v", got, want)
	}
}
