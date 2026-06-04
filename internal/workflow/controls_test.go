package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

func writeScript(t *testing.T, body string) (dir, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "s.star")
	src := "meta = {\"name\": \"n\", \"description\": \"d\"}\n" + body + "\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

// TestPersistIOPlumbing: persist-io is default-ON (the leaf request carries PersistIO=true
// + the prompt as IOPrompt), and --no-persist-io (Options.NoPersistIO) turns it off.
func TestPersistIOPlumbing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want bool
	}{
		{"default-on", Options{}, true},
		{"opt-out", Options{NoPersistIO: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			rec := &recorder{}
			old := runLeaf
			runLeaf = echoLeaf(rec)
			t.Cleanup(func() { runLeaf = old })
			_, script := writeScript(t, `x = agent("hello", vendor="v")`)
			run, err := Prepare(script)
			if err != nil {
				t.Fatal(err)
			}
			if err := Execute(context.Background(), script, run.RunID, tc.opts); err != nil {
				t.Fatalf("execute: %v", err)
			}
			c := rec.snapshot()[0]
			if c.persistIO != tc.want {
				t.Errorf("req.PersistIO = %v, want %v", c.persistIO, tc.want)
			}
			if c.ioPrompt != "hello" {
				t.Errorf("req.IOPrompt = %q, want hello (the engine passes the prompt for the side file)", c.ioPrompt)
			}
		})
	}
}

// TestMetaModelFallbackAndWhenToUse: meta.model is the default model for agents that omit
// model= (and reaches the leaf request), an explicit model= overrides it, and
// meta.whenToUse lands on the manifest.
func TestMetaModelFallbackAndWhenToUse(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	script := filepath.Join(t.TempDir(), "m.star")
	full := `meta = {"name": "n", "description": "d", "model": "meta-default", "whenToUse": "for audits"}
a = agent("uses default", vendor="v")
b = agent("overrides", vendor="v", model="explicit")
`
	if err := os.WriteFile(script, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	byPrompt := map[string]leafCall{}
	for _, c := range rec.snapshot() {
		byPrompt[c.prompt] = c
	}
	if m := byPrompt["uses default"].model; m != "meta-default" {
		t.Errorf("omitted model = %q, want meta-default (meta.model fallback)", m)
	}
	if m := byPrompt["overrides"].model; m != "explicit" {
		t.Errorf("explicit model = %q, want explicit (overrides meta.model)", m)
	}
	if got, _ := subagent.ReadRun(run.RunID); got.WhenToUse != "for audits" {
		t.Errorf("manifest WhenToUse = %q, want 'for audits'", got.WhenToUse)
	}
}

// TestEnginePIDAndScriptPersisted: Launch persists runs/<id>.star (for restart), and a
// DETACHED-style Execute (opts.RunID set) records EnginePID so `workflow stop` can reap
// it; a foreground run (opts.RunID empty) records no pid.
func TestEnginePIDAndScriptPersisted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rec := &recorder{}
	old := runLeaf
	runLeaf = echoLeaf(rec)
	t.Cleanup(func() { runLeaf = old })
	_, script := writeScript(t, `x = agent("hi", vendor="v")`)

	id, err := Launch(context.Background(), script, Options{}, true) // foreground
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	sp, _ := subagent.RunScriptPath(id)
	if _, serr := os.Stat(sp); serr != nil {
		t.Errorf(".star not persisted at Launch: %v", serr)
	}
	if got, _ := subagent.ReadRun(id); got.EnginePID != 0 {
		t.Errorf("foreground run recorded EnginePID=%d, want 0 (not stop-reapable)", got.EnginePID)
	}

	run, err := Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{RunID: run.RunID}); err != nil {
		t.Fatalf("detached execute: %v", err)
	}
	if got, _ := subagent.ReadRun(run.RunID); got.EnginePID != os.Getpid() {
		t.Errorf("detached EnginePID = %d, want %d", got.EnginePID, os.Getpid())
	}
}
