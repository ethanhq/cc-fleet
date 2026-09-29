package teammate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// defsConfig builds a config from providers keyed by name.
func defsConfig(providers ...*config.Provider) *config.Config {
	cfg := &config.Config{Version: 1, Providers: map[string]*config.Provider{}}
	for _, p := range providers {
		cfg.Providers[p.Name] = p
	}
	return cfg
}

// defsAgentsDir returns the hermetic claudepaths.Agents() directory, created.
func defsAgentsDir(t *testing.T, h testHome) string {
	t.Helper()
	dir := filepath.Join(h.ClaudeDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// defsDescription returns the decoded description: value of a rendered definition.
func defsDescription(t *testing.T, def []byte) string {
	t.Helper()
	for _, line := range strings.Split(string(def), "\n") {
		if v, ok := strings.CutPrefix(line, "description: "); ok {
			var s string
			if err := json.Unmarshal([]byte(v), &s); err != nil {
				t.Fatalf("description %q is not a plain double-quoted scalar: %v", v, err)
			}
			return s
		}
	}
	t.Fatalf("no description line in\n%s", def)
	return ""
}

// defsList returns the sorted file names in dir.
func defsList(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRenderAgentDef(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "defs", "ccf-glm.strong.md"))
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")) // a Windows checkout may convert line endings
	strong := AgentType{Provider: "glm", Slot: SlotStrong}
	got := RenderAgentDef(strong, "glm", "glm-4.6[1m]")
	if !bytes.Equal(got, want) {
		t.Fatalf("RenderAgentDef differs from the design format:\n--- got\n%s\n--- want\n%s", got, want)
	}

	def := RenderAgentDef(AgentType{Provider: "p"}, "p", "m")
	lines := strings.Split(string(def), "\n")
	if lines[1] != "name: ccf-p" || lines[3] != "model: ccf-p" {
		t.Fatalf("name/model lines = %q, %q; the model must be the sentinel type name", lines[1], lines[3])
	}
	if lines[4] != "---" || lines[5] != AgentDefMarker {
		t.Fatalf("the marker must be the first body line, got %q", lines[5])
	}

	tricky := RenderAgentDef(AgentType{Provider: "p"}, "p", "m\"x\\y\n\tz\x7f\u2028\u0085[1m]")
	if n := strings.Count(string(tricky), "\n"); n != 7 {
		t.Fatalf("control characters leaked a line break: %d lines\n%s", n, tricky)
	}
	desc := defsDescription(t, tricky)
	if !strings.Contains(desc, `(p, m"x\yz).`) {
		t.Fatalf("description %q: quotes and backslashes must round-trip, controls dropped, [1m] stripped", desc)
	}
	if !strings.Contains(desc, `subagent_type: "ccf-p"`) {
		t.Fatalf("description %q lacks the subagent_type hint", desc)
	}
}

func TestSyncWritesSlotDefsOnlyWhenDifferent(t *testing.T) {
	h := hermeticHome(t)
	cfg := defsConfig(
		&config.Provider{Name: "alpha", Enabled: true, DefaultModel: "m[1m]", StrongModel: "m", FastModel: "f"},
		&config.Provider{Name: "beta", Enabled: true, DefaultModel: "b", StrongModel: "b-big[1m]"},
		&config.Provider{Name: "gamma", Enabled: true, DefaultModel: "g", StrongModel: "g", FastModel: "g[1m]"},
	)
	res, err := SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []string{"ccf-alpha.fast.md", "ccf-alpha.md", "ccf-beta.md", "ccf-beta.strong.md", "ccf-gamma.md"}
	if got := defsList(t, filepath.Join(h.ClaudeDir, "agents")); !reflect.DeepEqual(got, wantFiles) {
		t.Fatalf("agents dir = %v, want %v", got, wantFiles)
	}
	wantRes := SyncResult{
		Written:   []string{"ccf-alpha.md", "ccf-alpha.fast.md", "ccf-beta.md", "ccf-beta.strong.md", "ccf-gamma.md"},
		Removed:   []string{},
		Unchanged: []string{},
	}
	if !reflect.DeepEqual(res, wantRes) {
		t.Fatalf("SyncResult = %+v, want %+v", res, wantRes)
	}
	data, err := os.ReadFile(filepath.Join(h.ClaudeDir, "agents", "ccf-beta.strong.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, RenderAgentDef(AgentType{Provider: "beta", Slot: SlotStrong}, "beta", "b-big[1m]")) {
		t.Fatalf("ccf-beta.strong.md:\n%s", data)
	}
	fi, err := os.Stat(filepath.Join(h.ClaudeDir, "agents", "ccf-alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 { // Windows has no POSIX modes
		t.Fatalf("definition mode = %v; want 0644", fi.Mode().Perm())
	}
}

func TestSyncSkipsDisabledAndReserved(t *testing.T) {
	h := hermeticHome(t)
	cfg := defsConfig(
		&config.Provider{Name: "off", Enabled: false, DefaultModel: "m", StrongModel: "s"},
		&config.Provider{Name: config.ReservedNativeProvider, Enabled: true, DefaultModel: "opus"},
		&config.Provider{Name: "on", Enabled: true, DefaultModel: "m"},
	)
	res, err := SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Written, []string{"ccf-on.md"}) {
		t.Fatalf("Written = %v, want only ccf-on.md", res.Written)
	}
	if got := defsList(t, filepath.Join(h.ClaudeDir, "agents")); !reflect.DeepEqual(got, []string{"ccf-on.md"}) {
		t.Fatalf("agents dir = %v", got)
	}

	// Nothing enabled: no directory is created, nothing is reported.
	h2 := hermeticHome(t)
	res, err = SyncAgentDefs(defsConfig(&config.Provider{Name: "off", DefaultModel: "m"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written)+len(res.Removed)+len(res.Unchanged)+len(res.Conflicts) != 0 {
		t.Fatalf("empty sync reported %+v", res)
	}
	if _, err := os.Stat(filepath.Join(h2.ClaudeDir, "agents")); !os.IsNotExist(err) {
		t.Fatalf("empty sync created the agents dir: %v", err)
	}
	if _, err := SyncAgentDefs(nil); err == nil {
		t.Fatal("a nil config must be an error, not a wipe")
	}
}

func TestSyncRemovesStaleManaged(t *testing.T) {
	h := hermeticHome(t)
	dir := defsAgentsDir(t, h)
	old := RenderAgentDef(AgentType{Provider: "old"}, "old", "m")
	if err := os.WriteFile(filepath.Join(dir, "ccf-old.md"), old, 0o644); err != nil {
		t.Fatal(err)
	}
	userDef := []byte("---\nname: mine\n---\nmine\n")
	if err := os.WriteFile(filepath.Join(dir, "mine.md"), userDef, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := defsConfig(&config.Provider{Name: "p", Enabled: true, DefaultModel: "m", StrongModel: "s"})
	res, err := SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := SyncResult{Written: []string{"ccf-p.md", "ccf-p.strong.md"}, Removed: []string{"ccf-old.md"}, Unchanged: []string{}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("first SyncResult = %+v, want %+v", res, want)
	}
	cfg.Providers["p"].StrongModel = "" // the strong slot goes away
	cfg.Providers["p"].DefaultModel = "m2"
	res, err = SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want = SyncResult{Written: []string{"ccf-p.md"}, Removed: []string{"ccf-p.strong.md"}, Unchanged: []string{}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("SyncResult = %+v, want %+v", res, want)
	}
	if got := defsList(t, dir); !reflect.DeepEqual(got, []string{"ccf-p.md", "mine.md"}) {
		t.Fatalf("agents dir = %v", got)
	}

	// A no-op sync leaves the files alone (same mtime) and reports them unchanged.
	path := filepath.Join(dir, "ccf-p.md")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	res, err = SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, SyncResult{Written: []string{}, Removed: []string{}, Unchanged: []string{"ccf-p.md"}}) {
		t.Fatalf("no-op SyncResult = %+v", res)
	}
	if fi, err := os.Stat(path); err != nil || !fi.ModTime().Equal(past) {
		t.Fatalf("no-op sync rewrote %s", path)
	}
}

func TestSyncNeverTouchesUnmarked(t *testing.T) {
	h := hermeticHome(t)
	dir := defsAgentsDir(t, h)
	foreign := []byte("---\nname: ccf-p\nmodel: sonnet\n---\nmy own agent\n")
	stale := []byte("---\nname: ccf-gone\n---\nnot ours either\n")
	for name, data := range map[string][]byte{"ccf-p.md": foreign, "ccf-gone.md": stale} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "ccf-q.md"), 0o755); err != nil { // not a regular file
		t.Fatal(err)
	}
	cfg := defsConfig(
		&config.Provider{Name: "p", Enabled: true, DefaultModel: "m"},
		&config.Provider{Name: "q", Enabled: true, DefaultModel: "m"},
	)
	res, err := SyncAgentDefs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := SyncResult{Written: []string{}, Removed: []string{}, Unchanged: []string{}, Conflicts: []string{"ccf-p.md", "ccf-q.md"}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("SyncResult = %+v, want %+v", res, want)
	}
	for name, data := range map[string][]byte{"ccf-p.md": foreign, "ccf-gone.md": stale} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s was changed: %q, %v", name, got, err)
		}
	}
	if IsManagedDef(filepath.Join(dir, "ccf-p.md")) || IsManagedDef(filepath.Join(dir, "ccf-q.md")) {
		t.Fatal("unmarked files and directories are not managed")
	}
}

func TestRemoveOnlyManaged(t *testing.T) {
	h := hermeticHome(t)

	res, err := RemoveAgentDefs() // no agents dir yet
	if err != nil || len(res.Removed) != 0 {
		t.Fatalf("remove without agents dir: %+v, %v", res, err)
	}

	dir := defsAgentsDir(t, h)
	cfg := defsConfig(&config.Provider{Name: "p", Enabled: true, DefaultModel: "m", FastModel: "f"})
	if _, err := SyncAgentDefs(cfg); err != nil {
		t.Fatal(err)
	}
	unmarked := []byte("---\nname: ccf-x\n---\nmine\n")
	if err := os.WriteFile(filepath.Join(dir, "ccf-x.md"), unmarked, 0o644); err != nil {
		t.Fatal(err)
	}
	// Carries the marker but is outside the ccf-*.md namespace.
	if err := os.WriteFile(filepath.Join(dir, "copy.md"), RenderAgentDef(AgentType{Provider: "p"}, "p", "m"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err = RemoveAgentDefs()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, SyncResult{Written: []string{}, Removed: []string{"ccf-p.fast.md", "ccf-p.md"}, Unchanged: []string{}}) {
		t.Fatalf("RemoveAgentDefs = %+v", res)
	}
	if got := defsList(t, dir); !reflect.DeepEqual(got, []string{"ccf-x.md", "copy.md"}) {
		t.Fatalf("agents dir = %v", got)
	}
}

func TestFindShadowingDefsByFrontmatterName(t *testing.T) {
	h := hermeticHome(t)
	write := func(path, content string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const typ = "ccf-glm"

	outside := filepath.Join(h.Home, "work")
	repo := filepath.Join(outside, "repo")
	cwd := filepath.Join(repo, "sub", "pkg")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	byName := write(filepath.Join(repo, ".claude", "agents", "team", "helper.md"), "---\ndescription: x\nname: \"ccf-glm\"\n---\nbody\n")
	atCwd := write(filepath.Join(cwd, ".claude", "agents", "a.md"), "---\nname: ccf-glm\nmodel: sonnet\n---\n")
	write(filepath.Join(repo, ".claude", "agents", "ccf-glm.md"), "---\nname: ccf-glm.strong\n---\n")    // file name matches, name does not
	write(filepath.Join(repo, ".claude", "agents", "nofront.md"), "name: ccf-glm\n")                     // no frontmatter
	write(filepath.Join(repo, ".claude", "agents", "body.md"), "---\nname: other\n---\nname: ccf-glm\n") // name only in the body
	write(filepath.Join(repo, ".claude", "agents", "nested.md"), "---\nmeta:\n  name: ccf-glm\n---\n")   // not a top-level key
	write(filepath.Join(repo, ".claude", "agents", "notes.txt"), "---\nname: ccf-glm\n---\n")            // not .md
	write(filepath.Join(outside, ".claude", "agents", "above-root.md"), "---\nname: ccf-glm\n---\n")     // beyond the git root
	write(filepath.Join(cwd, ".claude", "agents", "crlf.md"), "\ufeff---\r\nname: 'ccf-glm'\r\n---\r\n") // BOM + CRLF + single quotes

	got := FindShadowingDefs(cwd, typ)
	want := []string{atCwd, filepath.Join(cwd, ".claude", "agents", "crlf.md"), byName}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FindShadowingDefs = %v, want %v", got, want)
	}
	if got := FindShadowingDefs(cwd, "ccf-other"); len(got) != 0 {
		t.Fatalf("no definition is named ccf-other, got %v", got)
	}

	// A .git file (worktree) is a git root too.
	wt := filepath.Join(h.Home, "wt")
	write(filepath.Join(wt, ".git"), "gitdir: /elsewhere\n")
	inWt := write(filepath.Join(wt, ".claude", "agents", "x.md"), "---\nname: ccf-glm\n---\n")
	if got := FindShadowingDefs(filepath.Join(wt, "deep"), typ); !reflect.DeepEqual(got, []string{inWt}) {
		t.Fatalf("worktree scan = %v, want %v", got, []string{inWt})
	}

	// Without a git root only leadCwd itself is scanned.
	plain := filepath.Join(h.Home, "plain", "child")
	write(filepath.Join(h.Home, "plain", ".claude", "agents", "parent.md"), "---\nname: ccf-glm\n---\n")
	self := write(filepath.Join(plain, ".claude", "agents", "self.md"), "---\nname: ccf-glm\n---\n")
	if got := FindShadowingDefs(plain, typ); !reflect.DeepEqual(got, []string{self}) {
		t.Fatalf("no-git scan = %v, want %v", got, []string{self})
	}

	// The user-level agents dir is never reported, even when a level's .claude/agents is it.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(plain, ".claude"))
	if got := FindShadowingDefs(plain, typ); len(got) != 0 {
		t.Fatalf("user-level agents reported as shadowing: %v", got)
	}
}

// TestFxtmFindShadowingDefsFollowsDirSymlinks: Claude Code's loader follows
// directory symlinks under .claude/agents, so the scan does too, without looping.
func TestFxtmFindShadowingDefsFollowsDirSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	h := hermeticHome(t)
	const typ = "ccf-x"
	shared := filepath.Join(h.Home, "shared")
	nested := filepath.Join(shared, "deep", "def.md")
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("---\nname: ccf-x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(h.Home, "proj")
	agents := filepath.Join(proj, ".claude", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"shared": shared,                               // directory outside the project
		"self":   filepath.Join(agents, "self"),        // self loop
		"up":     agents,                               // back to the scanned root
		"back":   shared,                               // the same directory again
		"user":   filepath.Join(h.ClaudeDir, "agents"), // the user-level dir is never scanned
	} {
		if err := os.Symlink(target, filepath.Join(agents, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shared, filepath.Join(shared, "deep", "loop")); err != nil {
		t.Fatal(err)
	}
	userDefs := defsAgentsDir(t, h)
	if err := os.WriteFile(filepath.Join(userDefs, "ccf-x.md"), []byte("---\nname: ccf-x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := FindShadowingDefs(proj, typ)
	if len(got) != 1 {
		t.Fatalf("FindShadowingDefs = %v, want the one definition in the linked directory", got)
	}
	if real, err := filepath.EvalSymlinks(got[0]); err != nil || real != nested {
		t.Fatalf("reported %q (resolves to %q, %v), want %q", got[0], real, err, nested)
	}
}

// TestFx2tmFrontmatterNameYAMLScalars: the name value is read as a YAML scalar
// (quotes, escapes, trailing comments), so a valid definition written in any of
// those forms is still found as a shadow.
func TestFx2tmFrontmatterNameYAMLScalars(t *testing.T) {
	h := hermeticHome(t)
	agents := filepath.Join(h.Home, "proj", ".claude", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ line, want string }{
		{`name: ccf-x`, "ccf-x"},
		{`name: ccf-x # shared`, "ccf-x"},
		{"name: ccf-x\t# tab", "ccf-x"},
		{`name: ccf-x#tag`, "ccf-x#tag"},
		{`name: # empty`, ""},
		{`name:   ccf-x   `, "ccf-x"},
		{`name: "ccf-x"`, "ccf-x"},
		{`name: "ccf-x" # c`, "ccf-x"},
		{`name: "ccf-x # not"`, "ccf-x # not"},
		{`name: "ccf\x2dx"`, "ccf-x"},
		{`name: "ccf-x"`, "ccf-x"},
		{`name: "ccf-\"x\""`, `ccf-"x"`},
		{`name: "a\\b\tc"`, "a\\b\tc"},
		{`name: "unterminated`, ""},
		{`name: 'ccf-x'`, "ccf-x"},
		{`name: 'ccf-x' # c`, "ccf-x"},
		{`name: 'it''s'`, "it's"},
		{`name: 'ccf-\x'`, `ccf-\x`},
		{`name: 'unterminated`, ""},
	}
	for i, c := range cases {
		path := filepath.Join(agents, fmt.Sprintf("c%d.md", i))
		body := "---\ndescription: a definition\n" + c.line + "\nmodel: sonnet\n---\nbody\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := frontmatterName(path); got != c.want {
			t.Errorf("%s: frontmatterName = %q, want %q", c.line, got, c.want)
		}
	}

	// Real, valid definitions using these forms are all reported as shadowing.
	proj := filepath.Join(h.Home, "real")
	dir := filepath.Join(proj, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var want []string
	for i, line := range []string{
		`name: ccf-x # shared`,
		`name: "ccf-x"`,
		`name: 'ccf-x'`,
		`name: "ccf\x2dx"`,
	} {
		path := filepath.Join(dir, fmt.Sprintf("d%d.md", i))
		body := "---\n" + line + "\ndescription: \"Reviews code\"\nmodel: sonnet\ntools: Read, Grep\n---\nYou review code.\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		want = append(want, path)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.md"), []byte("---\nname: ccf-x#tag\nmodel: sonnet\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindShadowingDefs(proj, "ccf-x"); !reflect.DeepEqual(got, want) {
		t.Fatalf("FindShadowingDefs = %v, want %v", got, want)
	}
}
