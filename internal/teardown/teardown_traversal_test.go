package teardown

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTeardownTeam_RejectsPathTraversal is the load-bearing path-traversal
// regression proof. `cc-fleet teardown ../..` must not delete the temp HOME and
// return ok:true. Every traversal-shaped name must:
//
//  1. fail with !OK (caller never sees a false success), and
//  2. leave a sentinel marker we plant inside the temp HOME completely intact
//     (the team root, and any neighbouring file, must be untouched).
//
// The test is hermetic: every path operation happens under t.TempDir(), no
// real ~/.claude is touched.
func TestTeardownTeam_RejectsPathTraversal(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// Plant a sentinel BEFORE invoking teardown. If the traversal hole still
	// exists, RemoveAll would wipe the temp HOME and the sentinel would be
	// gone after the call.
	sentinelDir := filepath.Join(tmpHome, "sentinel-dir")
	sentinelFile := filepath.Join(sentinelDir, "DO-NOT-DELETE.txt")
	if err := os.MkdirAll(sentinelDir, 0o700); err != nil {
		t.Fatalf("setup sentinel dir: %v", err)
	}
	if err := os.WriteFile(sentinelFile, []byte("preserve me"), 0o600); err != nil {
		t.Fatalf("setup sentinel file: %v", err)
	}

	// Also create the canonical teams root + an innocent neighbour team so the
	// proof holds even when a real team tree exists alongside the malicious
	// name. The neighbour must be untouched.
	teamsRoot := filepath.Join(tmpHome, ".claude", "teams")
	innocent := filepath.Join(teamsRoot, "innocent")
	if err := os.MkdirAll(innocent, 0o700); err != nil {
		t.Fatalf("setup innocent team: %v", err)
	}
	innocentMarker := filepath.Join(innocent, "config.json")
	if err := os.WriteFile(innocentMarker, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write innocent marker: %v", err)
	}

	traversalCases := []string{
		"..",
		"../..",
		"../../etc",
		"a/b",
		"/abs",
		`back\slash`,
		".",
	}
	for _, name := range traversalCases {
		name := name
		t.Run("name="+name, func(t *testing.T) {
			res := TeardownTeam(name, nil)
			if res.OK {
				t.Fatalf("TeardownTeam(%q): want OK=false, got OK=true (path traversal not blocked!)", name)
			}
			if res.TeamRemoved {
				t.Fatalf("TeardownTeam(%q): team_removed=true must be false on a rejected name", name)
			}
			// Sentinel + neighbour MUST still exist regardless of which form
			// failed; if the validator was missing, RemoveAll could nuke them.
			if _, err := os.Stat(sentinelFile); err != nil {
				t.Fatalf("sentinel file removed by malicious teardown %q: %v", name, err)
			}
			if _, err := os.Stat(innocentMarker); err != nil {
				t.Fatalf("innocent team's config.json removed by malicious teardown %q: %v", name, err)
			}
		})
	}
}

// TestTeardownTeam_AcceptsLegitName: control case proving the validator
// hasn't broken normal usage. Create a fake team, teardown removes it
// successfully.
func TestTeardownTeam_AcceptsLegitName(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	teamDir := filepath.Join(tmpHome, ".claude", "teams", "real-team")
	if err := os.MkdirAll(teamDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfgPath := filepath.Join(teamDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"members":[]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	res := TeardownTeam("real-team", nil)
	if !res.OK {
		t.Fatalf("TeardownTeam(real-team): code=%s msg=%s — should accept normal name",
			res.ErrorCode, res.ErrorMsg)
	}
	if !res.TeamRemoved {
		t.Fatalf("TeardownTeam(real-team): team_removed=false; want true after legit cleanup")
	}
	if _, err := os.Stat(teamDir); !os.IsNotExist(err) {
		t.Fatalf("team dir still exists after teardown: err=%v", err)
	}
}

// TestSpawnPaths_RejectInvalidNames: spawn-side low-level helpers (TeamDir /
// InboxPath) must also reject before constructing a path so a future caller
// that bypassed the CLI validator (e.g. a programmatic API user) cannot
// trigger the bug.
func TestSpawnPaths_RejectInvalidNames(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	// Import alias avoids the spawn package's own self-test paths.

	// We exercise the helpers via teardown — but teardown only takes the
	// team name. To test InboxPath directly we'd need to import spawn, but
	// that creates a test-time dep cycle on internal/spawn for a package
	// whose normal call site already returns errors. Coverage here is the
	// boundary test against teardown's RemoveAll, which is the load-bearing
	// surface for the path-traversal guard. InboxPath / TeamDir name validation
	// is covered by internal/spawn unit tests.
	for _, bad := range []string{"..", "../x", "/abs"} {
		res := TeardownTeam(bad, nil)
		if res.OK {
			t.Fatalf("TeardownTeam(%q): OK=true; validator missing for low-level path build", bad)
		}
	}
}
