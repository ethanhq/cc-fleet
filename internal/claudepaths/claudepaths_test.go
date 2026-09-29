package claudepaths

import (
	"path/filepath"
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
