package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSlimValidateProfile(t *testing.T) {
	for _, p := range []string{"", "full", "slim", "slim-ro"} {
		if err := ValidateProfile(p); err != nil {
			t.Errorf("ValidateProfile(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"FULL", "Slim", "slimro", "readonly", "x"} {
		if err := ValidateProfile(p); err == nil {
			t.Errorf("ValidateProfile(%q) = nil, want error", p)
		}
	}
}

func TestSlimDefaultTools(t *testing.T) {
	cases := []struct {
		profile  string
		noSkills bool
		want     []string
	}{
		{ProfileSlim, false, []string{"Bash", "Read", "Edit", "Write", "Grep", "Glob", "Skill"}},
		{ProfileSlim, true, []string{"Bash", "Read", "Edit", "Write", "Grep", "Glob"}},
		{ProfileSlimRO, false, []string{"Bash", "Read", "Grep", "Glob", "Skill"}},
		{ProfileSlimRO, true, []string{"Bash", "Read", "Grep", "Glob"}},
	}
	for _, c := range cases {
		got := DefaultSlimTools(c.profile, c.noSkills)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("DefaultSlimTools(%q, %v) = %v, want %v", c.profile, c.noSkills, got, c.want)
		}
	}
}

func TestSlimCanonicalizeTools(t *testing.T) {
	// Dedupe + sort, caller order irrelevant.
	got, err := CanonicalizeTools([]string{"Read", "Bash", "Grep"})
	if err != nil {
		t.Fatalf("CanonicalizeTools: %v", err)
	}
	want := []string{"Bash", "Grep", "Read"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sorted = %v, want %v", got, want)
	}

	// Order-insensitivity: a permuted input yields the same result.
	got2, err := CanonicalizeTools([]string{"Grep", "Read", "Bash"})
	if err != nil {
		t.Fatalf("CanonicalizeTools permuted: %v", err)
	}
	if !reflect.DeepEqual(got2, want) {
		t.Errorf("permuted result = %v, want %v (order must not matter)", got2, want)
	}

	// Every canonical name validates and the default sets round-trip.
	for _, profile := range []string{ProfileSlim, ProfileSlimRO} {
		if _, err := CanonicalizeTools(DefaultSlimTools(profile, false)); err != nil {
			t.Errorf("default tools for %q rejected: %v", profile, err)
		}
	}

	// Errors: unknown, duplicate, empty.
	if _, err := CanonicalizeTools([]string{"Read", "Nope"}); err == nil {
		t.Error("unknown tool accepted, want error")
	}
	if _, err := CanonicalizeTools([]string{"Read", "Read"}); err == nil {
		t.Error("duplicate tool accepted, want error")
	}
	if _, err := CanonicalizeTools([]string{"Read", ""}); err == nil {
		t.Error("empty tool accepted, want error")
	}
}

func TestValidateToolsSkills(t *testing.T) {
	// Skill in an explicit set + skills disabled is the one contradiction. This is the
	// shared check the bare-CLI front-load, Run, and the workflow engine all call.
	if err := ValidateToolsSkills([]string{"Read", "Skill"}, true); err == nil {
		t.Error("Skill + NoSkills must be rejected as contradictory")
	}
	// Consistent combinations pass.
	for _, c := range []struct {
		tools    []string
		noSkills bool
	}{
		{[]string{"Read", "Skill"}, false}, // skills on → Skill is allowed
		{[]string{"Read", "Grep"}, true},   // skills off, no Skill named → fine
		{nil, true},                        // no explicit tools → profile default drops Skill itself
		{[]string{"Read"}, false},
	} {
		if err := ValidateToolsSkills(c.tools, c.noSkills); err != nil {
			t.Errorf("ValidateToolsSkills(%v, %v) = %v, want nil", c.tools, c.noSkills, err)
		}
	}
}

func TestSlimRenderRejectsNonSlimProfile(t *testing.T) {
	for _, p := range []string{"", "full", "bogus"} {
		if _, err := RenderSlimPrompt(p, "", "m"); err == nil {
			t.Errorf("RenderSlimPrompt(%q) = nil error, want error", p)
		}
	}
}

const agentPromptMarker = "You are an agent for Claude Code, Anthropic's official CLI for Claude."

func TestSlimRenderCommonMarkers(t *testing.T) {
	t.Setenv("SHELL", "/bin/zsh") // deterministic Shell line
	dir := t.TempDir()            // not a git repo
	for _, profile := range []string{ProfileSlim, ProfileSlimRO} {
		out, err := RenderSlimPrompt(profile, dir, "vendor-model-x")
		if err != nil {
			t.Fatalf("RenderSlimPrompt(%q): %v", profile, err)
		}
		mustContain(t, profile, out,
			"You are Claude Code, Anthropic's official CLI for Claude.",
			agentPromptMarker,
			"Notes:",
			"please only use absolute file paths.",
			"Do NOT Write report/summary/findings/analysis .md files.",
			"<env>",
			"Working directory: "+dir,
			"Is directory a git repo: No",
			"Shell: zsh",
			"Today's date: "+time.Now().Format("2006-01-02"),
			"You are powered by the model named vendor-model-x.",
		)
		// Identity line and the agent paragraph must be separated, not concatenated into
		// "...CLI for Claude.You are an agent...".
		if strings.Contains(out, "CLI for Claude.You are an agent") {
			t.Errorf("%q render concatenated the identity line and the agent paragraph", profile)
		}
		// No knowledge-cutoff line (vendor models — accepted deviation).
		if strings.Contains(out, "knowledge cutoff") {
			t.Errorf("%q render leaked a knowledge-cutoff line", profile)
		}
	}
}

func TestSlimRenderShellFallback(t *testing.T) {
	t.Setenv("SHELL", "")
	out, err := RenderSlimPrompt(ProfileSlim, t.TempDir(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Shell: unknown") {
		t.Error("an unset $SHELL must render the 'unknown' fallback")
	}
}

func TestSlimRenderReadOnlyParagraph(t *testing.T) {
	const marker = "read-only research agent"
	dir := t.TempDir()

	ro, err := RenderSlimPrompt(ProfileSlimRO, dir, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ro, marker) {
		t.Error("slim-ro render missing the read-only research paragraph")
	}

	rw, err := RenderSlimPrompt(ProfileSlim, dir, "m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rw, marker) {
		t.Error("slim render must not carry the read-only research paragraph")
	}
}

func TestSlimRenderGitStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := newGitRepo(t)

	slim, err := RenderSlimPrompt(ProfileSlim, repo, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(slim, "Is directory a git repo: Yes") {
		t.Error("slim render in a git repo missing the git-repo env marker")
	}
	if !strings.Contains(slim, "gitStatus:") || !strings.Contains(slim, "Current branch:") {
		t.Error("slim render in a git repo missing the gitStatus block")
	}

	// slim-ro never emits gitStatus, even inside a git repo.
	ro, err := RenderSlimPrompt(ProfileSlimRO, repo, "m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ro, "gitStatus:") {
		t.Error("slim-ro render must omit gitStatus even in a git repo")
	}

	// slim outside a git repo omits gitStatus.
	nongit := t.TempDir()
	out, err := RenderSlimPrompt(ProfileSlim, nongit, "m")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "gitStatus:") {
		t.Error("slim render outside a git repo must omit gitStatus")
	}
}

func mustContain(t *testing.T, profile, out string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if !strings.Contains(out, m) {
			t.Errorf("%q render missing marker %q", profile, m)
		}
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return dir
}
