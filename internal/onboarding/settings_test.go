package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func setWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // defeat umask
		t.Fatal(err)
	}
}

func setRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func setCompact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %q: %v", raw, err)
	}
	return buf.String()
}

// setRawObject decodes a JSON object into ordered keys and raw values.
func setRawObject(t *testing.T, data []byte) ([]string, map[string]json.RawMessage) {
	t.Helper()
	obj, err := decodeSettingsObject(data)
	if err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for _, e := range obj {
		keys = append(keys, e.key)
		vals[e.key] = e.val
	}
	return keys, vals
}

func setSkipNoUnixModes(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no unix mode bits / symlinks without privilege on windows")
	}
}

func TestEditSettingsSymlinkTarget(t *testing.T) {
	setSkipNoUnixModes(t)
	dir := t.TempDir()

	t.Run("existing target", func(t *testing.T) {
		real := filepath.Join(dir, "dotfiles", "settings.json")
		setWrite(t, real, `{"model":"opus"}`, 0o640)
		link := filepath.Join(dir, "claude", "settings.json")
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}

		changed, err := EditSettings(link, []SettingsEdit{{Path: []string{"teammateMode"}, Value: "tmux"}})
		if err != nil {
			t.Fatalf("EditSettings: %v", err)
		}
		if !reflect.DeepEqual(changed, []string{"teammateMode"}) {
			t.Fatalf("changed = %v", changed)
		}
		fi, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("symlink was replaced by a regular file")
		}
		if got, _ := os.Readlink(link); got != real {
			t.Fatalf("symlink now points to %q, want %q", got, real)
		}
		if got := setRead(t, real); got != "{\n  \"model\": \"opus\",\n  \"teammateMode\": \"tmux\"\n}\n" {
			t.Fatalf("target content = %q", got)
		}
		if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
			t.Fatalf("target mode = %o, want 0640", fi.Mode().Perm())
		}
		v, ok, err := SettingsString(link, "teammateMode")
		if err != nil || !ok || v != "tmux" {
			t.Fatalf("SettingsString via link = %q %v %v", v, ok, err)
		}
	})

	t.Run("dangling symlink", func(t *testing.T) {
		real := filepath.Join(dir, "elsewhere", "settings.json")
		if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "dangling.json")
		if err := os.Symlink("elsewhere/settings.json", link); err != nil {
			t.Fatal(err)
		}
		if _, err := EditSettings(link, []SettingsEdit{{Path: []string{"env", "K"}, Value: "v"}}); err != nil {
			t.Fatalf("EditSettings: %v", err)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("dangling symlink was replaced: %v", err)
		}
		if got := setRead(t, real); got != "{\n  \"env\": {\n    \"K\": \"v\"\n  }\n}\n" {
			t.Fatalf("target content = %q", got)
		}
		if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
			t.Fatalf("new target mode = %o, want 0600", fi.Mode().Perm())
		}
	})
}

func TestEditSettingsKeyOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	in := `{"zeta": 1.50, "alpha": {"b": [1, 2, {"x": null}], "a": 1e3},
	"env": {"ZZ": "1", "CLAUDE_CODE_TEAMMATE_COMMAND": "/old", "AA": "2"},
	"permissions":{"allow":["Bash(ls)"]}, "beta": true}`
	setWrite(t, path, in, 0o600)

	changed, err := EditSettings(path, []SettingsEdit{
		{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Value: "/new/shim"},
		{Path: []string{"env", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"}, Value: "1"},
		{Path: []string{"teammateMode"}, Value: "tmux"},
	})
	if err != nil {
		t.Fatalf("EditSettings: %v", err)
	}
	want := []string{"env.CLAUDE_CODE_TEAMMATE_COMMAND", "env.CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "teammateMode"}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}

	out := []byte(setRead(t, path))
	keys, vals := setRawObject(t, out)
	if want := []string{"zeta", "alpha", "env", "permissions", "beta", "teammateMode"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("top-level order = %v, want %v", keys, want)
	}
	envKeys, envVals := setRawObject(t, vals["env"])
	if want := []string{"ZZ", "CLAUDE_CODE_TEAMMATE_COMMAND", "AA", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"}; !reflect.DeepEqual(envKeys, want) {
		t.Fatalf("env order = %v, want %v", envKeys, want)
	}
	if got := string(envVals["CLAUDE_CODE_TEAMMATE_COMMAND"]); got != `"/new/shim"` {
		t.Fatalf("teammate command = %s", got)
	}

	// Untouched values are byte-identical after json.Compact.
	_, inVals := setRawObject(t, []byte(in))
	for _, k := range []string{"zeta", "alpha", "permissions", "beta"} {
		if a, b := setCompact(t, inVals[k]), setCompact(t, vals[k]); a != b {
			t.Fatalf("%s changed: %s -> %s", k, a, b)
		}
	}
	if setCompact(t, vals["zeta"]) != "1.50" || !strings.Contains(setCompact(t, vals["alpha"]), `"a":1e3`) {
		t.Fatalf("numbers were not kept verbatim: %s", out)
	}

	// Fixed 2-space indent and trailing newline.
	var want2 bytes.Buffer
	if err := json.Indent(&want2, []byte(setCompact(t, out)), "", "  "); err != nil {
		t.Fatal(err)
	}
	want2.WriteByte('\n')
	if string(out) != want2.String() {
		t.Fatalf("output is not 2-space indented with trailing newline:\n%s", out)
	}
}

func TestEditSettingsNoHTMLEscape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	setWrite(t, path, `{"statusLine":{"command":"a && b <c> d"},"escaped":"x\u0026y","env":{}}`, 0o600)

	if _, err := EditSettings(path, []SettingsEdit{
		{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Value: "/tmp/a&b/<shim>"},
	}); err != nil {
		t.Fatalf("EditSettings: %v", err)
	}
	out := setRead(t, path)
	for _, want := range []string{`"a && b <c> d"`, `"/tmp/a&b/<shim>"`, `"x\u0026y"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %s:\n%s", want, out)
		}
	}
	for _, bad := range []string{`\u003c`, `\u003e`} {
		if strings.Contains(out, bad) {
			t.Fatalf("output HTML-escaped (%s):\n%s", bad, out)
		}
	}
}

func TestEditSettingsPreservesMode(t *testing.T) {
	setSkipNoUnixModes(t)
	dir := t.TempDir()

	path := filepath.Join(dir, "settings.json")
	setWrite(t, path, `{"a":"b"}`, 0o644)
	if _, err := EditSettings(path, []SettingsEdit{{Path: []string{"a"}, Value: "c"}}); err != nil {
		t.Fatalf("EditSettings: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %o, want 0644 kept", fi.Mode().Perm())
	}

	fresh := filepath.Join(dir, "missing", ".claude", "settings.json")
	if _, err := EditSettings(fresh, []SettingsEdit{{Path: []string{"teammateMode"}, Value: "tmux"}}); err != nil {
		t.Fatalf("EditSettings on missing file: %v", err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(fresh)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("new dir mode = %o, want 0700", fi.Mode().Perm())
	}
	if got := setRead(t, fresh); got != "{\n  \"teammateMode\": \"tmux\"\n}\n" {
		t.Fatalf("new file = %q", got)
	}
}

func TestEditSettingsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	setWrite(t, path, `{"teammateMode":"tmux","env":{"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS":"1"}}`, 0o600)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before := setRead(t, path)

	changed, err := EditSettings(path, []SettingsEdit{
		{Path: []string{"teammateMode"}, Value: "tmux"},
		{Path: []string{"env", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"}, Value: "1"},
		{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Delete: true},
		{Path: []string{"nope", "deeper"}, Delete: true},
	})
	if err != nil {
		t.Fatalf("EditSettings: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v, want none", changed)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Fatalf("mtime changed: %v -> %v", old, fi.ModTime())
	}
	if setRead(t, path) != before {
		t.Fatal("content changed on a no-op edit")
	}

	// A second identical real edit is a no-op too.
	edit := []SettingsEdit{{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Value: "/shim"}}
	if changed, err := EditSettings(path, edit); err != nil || len(changed) != 1 {
		t.Fatalf("first edit: changed=%v err=%v", changed, err)
	}
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if changed, err := EditSettings(path, edit); err != nil || len(changed) != 0 {
		t.Fatalf("repeat edit: changed=%v err=%v", changed, err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Fatalf("repeat edit rewrote the file")
	}

	// Deleting from a missing file creates nothing.
	missing := filepath.Join(dir, "absent", "settings.json")
	if changed, err := EditSettings(missing, []SettingsEdit{{Path: []string{"teammateMode"}, Delete: true}}); err != nil || len(changed) != 0 {
		t.Fatalf("delete on missing: changed=%v err=%v", changed, err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-op edit created %s: %v", filepath.Dir(missing), err)
	}
}

func TestEditSettingsDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	setWrite(t, path, `{"a":1,"env":{"CLAUDE_CODE_TEAMMATE_COMMAND":"/shim","KEEP":"k"},"teammateMode":"tmux","z":2}`, 0o600)

	changed, err := EditSettings(path, []SettingsEdit{
		{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Delete: true},
		{Path: []string{"teammateMode"}, Delete: true},
		{Path: []string{"absent"}, Delete: true},
	})
	if err != nil {
		t.Fatalf("EditSettings: %v", err)
	}
	if want := []string{"env.CLAUDE_CODE_TEAMMATE_COMMAND", "teammateMode"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	want := "{\n  \"a\": 1,\n  \"env\": {\n    \"KEEP\": \"k\"\n  },\n  \"z\": 2\n}\n"
	if got := setRead(t, path); got != want {
		t.Fatalf("after delete:\n%s\nwant:\n%s", got, want)
	}

	// Deleting the last env key leaves an empty env object.
	if _, err := EditSettings(path, []SettingsEdit{{Path: []string{"env", "KEEP"}, Delete: true}}); err != nil {
		t.Fatal(err)
	}
	if _, present, err := SettingsString(path, "env", "KEEP"); err != nil || present {
		t.Fatalf("KEEP still present=%v err=%v", present, err)
	}
	if !strings.Contains(setRead(t, path), `"env": {}`) {
		t.Fatalf("env object not kept: %s", setRead(t, path))
	}
}

func TestEditSettingsNonObject(t *testing.T) {
	cases := map[string]string{
		"array":      `[1,2]`,
		"string":     `"x"`,
		"null":       `null`,
		"env string": `{"env":"x"}`,
		"env array":  `{"env":["A=1"]}`,
		"env null":   `{"env":null}`,
		"env number": `{"env":3}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			setWrite(t, path, content, 0o600)
			edits := []SettingsEdit{{Path: []string{"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, Value: "/shim"}}
			changed, err := EditSettings(path, edits)
			if !errors.Is(err, ErrSettingsShape) {
				t.Fatalf("err = %v, want ErrSettingsShape", err)
			}
			if changed != nil {
				t.Fatalf("changed = %v, want nil", changed)
			}
			if got := setRead(t, path); got != content {
				t.Fatalf("file modified: %q", got)
			}
			if _, _, err := SettingsString(path, "env", "CLAUDE_CODE_TEAMMATE_COMMAND"); !errors.Is(err, ErrSettingsShape) {
				t.Fatalf("SettingsString err = %v, want ErrSettingsShape", err)
			}
		})
	}

	t.Run("non-string leaf is replaced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		setWrite(t, path, `{"teammateMode":{"x":1}}`, 0o600)
		changed, err := EditSettings(path, []SettingsEdit{{Path: []string{"teammateMode"}, Value: "tmux"}})
		if err != nil || len(changed) != 1 {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		setWrite(t, path, `{"env":{`, 0o600)
		if _, err := EditSettings(path, []SettingsEdit{{Path: []string{"teammateMode"}, Value: "tmux"}}); err == nil {
			t.Fatal("want an error for malformed JSON")
		}
		if got := setRead(t, path); got != `{"env":{` {
			t.Fatalf("file modified: %q", got)
		}
	})
}

func TestSettingsString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	setWrite(t, path, `{"teammateMode":"auto","env":{"A":"1","N":2},"dup":"first","dup":"last"}`, 0o600)

	for _, tc := range []struct {
		keys    []string
		value   string
		present bool
	}{
		{[]string{"teammateMode"}, "auto", true},
		{[]string{"env", "A"}, "1", true},
		{[]string{"env", "N"}, "", false},
		{[]string{"env", "missing"}, "", false},
		{[]string{"nope"}, "", false},
		{[]string{"nope", "deeper"}, "", false},
		{[]string{"dup"}, "last", true},
	} {
		v, ok, err := SettingsString(path, tc.keys...)
		if err != nil || v != tc.value || ok != tc.present {
			t.Fatalf("SettingsString(%v) = %q %v %v, want %q %v", tc.keys, v, ok, err, tc.value, tc.present)
		}
	}
	if v, ok, err := SettingsString(filepath.Join(dir, "absent.json"), "teammateMode"); err != nil || ok || v != "" {
		t.Fatalf("missing file = %q %v %v", v, ok, err)
	}
	empty := filepath.Join(dir, "empty.json")
	setWrite(t, empty, "  \n", 0o600)
	if _, ok, err := SettingsString(empty, "teammateMode"); err != nil || ok {
		t.Fatalf("empty file = %v %v", ok, err)
	}
}

// TestFxtmSettingsStringNonString: JSON null (and any other non-string) is not present.
func TestFxtmSettingsStringNonString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	setWrite(t, path, `{"teammateMode":null,"env":{"CLAUDE_CODE_TEAMMATE_COMMAND": null,"B":true,"O":{},"L":[]}}`, 0o600)
	for _, keys := range [][]string{
		{"teammateMode"}, {"env", "CLAUDE_CODE_TEAMMATE_COMMAND"}, {"env", "B"}, {"env", "O"}, {"env", "L"},
	} {
		if v, ok, err := SettingsString(path, keys...); err != nil || ok || v != "" {
			t.Errorf("SettingsString(%v) = %q %v %v, want \"\" false nil", keys, v, ok, err)
		}
	}
}
