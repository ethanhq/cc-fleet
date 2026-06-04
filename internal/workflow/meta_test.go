package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

func TestExtractMeta(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr bool
		check   func(t *testing.T, m scriptMeta)
	}{
		{
			name: "valid with phases",
			src:  `meta = {"name": "build", "description": "do it", "phases": [{"title": "plan", "detail": "scope"}, {"title": "ship"}]}`,
			check: func(t *testing.T, m scriptMeta) {
				if m.Name != "build" || m.Description != "do it" {
					t.Errorf("name/desc = %q/%q", m.Name, m.Description)
				}
				if len(m.Phases) != 2 || m.Phases[0].Title != "plan" || m.Phases[0].Detail != "scope" || m.Phases[1].Title != "ship" {
					t.Errorf("phases = %+v", m.Phases)
				}
			},
		},
		{
			name:  "bool/None/negative values are literals",
			src:   `meta = {"name": "n", "description": "d", "x": True, "y": None, "z": -3}`,
			check: func(t *testing.T, m scriptMeta) {},
		},
		{name: "missing name", src: `meta = {"description": "d"}`, wantErr: true},
		{name: "empty name", src: `meta = {"name": "", "description": "d"}`, wantErr: true},
		{name: "missing description", src: `meta = {"name": "n"}`, wantErr: true},
		{name: "no meta at all", src: `x = 1`, wantErr: true},
		{name: "meta is not a dict", src: `meta = "hello"`, wantErr: true},
		{name: "name references a variable (not literal)", src: `V = "n"` + "\n" + `meta = {"name": V, "description": "d"}`, wantErr: true},
		{name: "value is a call (not literal)", src: `meta = {"name": "n", "description": str(1)}`, wantErr: true},
		{name: "phases not a list", src: `meta = {"name": "n", "description": "d", "phases": "plan"}`, wantErr: true},
		{name: "phase entry without title", src: `meta = {"name": "n", "description": "d", "phases": [{"detail": "x"}]}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := extractMeta(fileOptions, "t.star", tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got meta %+v", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, m)
			}
		})
	}
}

// TestPhaseDedupAgainstMeta (review F2): phase() must dedup against the FULL manifest
// phase set, including the titles minted from static meta — so a faithful script that
// declares phases in meta AND calls phase() for them never creates a duplicate row.
func TestPhaseDedupAgainstMeta(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	old := runLeaf
	runLeaf = func(subagent.Request) subagent.Result { return subagent.Result{OK: true, Result: "ok"} }
	t.Cleanup(func() { runLeaf = old })

	dir := t.TempDir()
	script := filepath.Join(dir, "wf.star")
	src := `meta = {"name": "n", "description": "d", "phases": [{"title": "plan"}, {"title": "build"}]}
phase("plan")
phase("ship")
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := Prepare(script)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := Execute(context.Background(), script, run.RunID, Options{}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got, err := subagent.ReadRun(run.RunID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	var titles []string
	for _, p := range got.Phases {
		titles = append(titles, p.Title)
	}
	want := []string{"plan", "build", "ship"} // plan deduped (meta+phase), ship appended
	if len(titles) != len(want) {
		t.Fatalf("phases = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Errorf("phases = %v, want %v", titles, want)
			break
		}
	}
	if got.Status != "done" {
		t.Errorf("status = %q, want done", got.Status)
	}
}

func TestDefaultConcurrencyFloor(t *testing.T) {
	if c := defaultConcurrency(); c < 1 || c > maxConcurrencyCap {
		t.Errorf("defaultConcurrency() = %d, want in [1,%d]", c, maxConcurrencyCap)
	}
}
