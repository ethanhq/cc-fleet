package subagent

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// ----- buildArgv: full stays byte-identical, slim appends -----

func TestBuildArgv_FullByteIdentical(t *testing.T) {
	const bin, prof, model = "/v/claude", "/p/glm.json", "glm-4.6"
	// The exact full argv (golden) — must not drift when slim is the zero value.
	want := []string{bin, "--dangerously-skip-permissions", "--settings", prof, "--model", model, "-p", "do it"}
	got := buildArgv(bin, prof, model, Request{Prompt: "do it"}, slimArgv{})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full argv drifted:\n got %v\nwant %v", got, want)
	}
	// A zero slimArgv adds no slim flags; a schema-less request adds no --json-schema.
	for _, f := range []string{"--system-prompt-file", "--tools", "--thinking", "--strict-mcp-config", "--json-schema"} {
		assertAbsent(t, got, f)
	}
}

func TestBuildArgv_Slim(t *testing.T) {
	const bin, prof, model = "/v/claude", "/p/glm.json", "glm-4.6"
	slim := slimArgv{promptFile: "/abs/job.slimprompt", tools: []string{"Bash", "Edit", "Read"}}

	t.Run("zero-value MCP keeps strict-mcp-config", func(t *testing.T) {
		// The user-facing boundaries resolve slim's MCP default to inherit; the
		// Request zero value stays strict for direct constructors.
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", PromptProfile: ProfileSlim}, slim)
		assertPairAfter(t, argv, "--system-prompt-file", "/abs/job.slimprompt")
		assertPairAfter(t, argv, "--tools", "Bash,Edit,Read")
		assertPairAfter(t, argv, "--thinking", "disabled")
		assertAbsent(t, argv, "default") // the join must be comma-separated, no literal "default"
		if idxOf(argv, "--strict-mcp-config") < 0 {
			t.Fatalf("zero-value MCP must carry --strict-mcp-config: %v", argv)
		}
		// Slim flags are APPENDED — the full prefix is unchanged.
		assertSeq(t, argv, bin, "--dangerously-skip-permissions", "--settings", prof, "--model", model, "-p", "x")
	})

	t.Run("mcp=true omits strict-mcp-config", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", PromptProfile: ProfileSlim, MCP: true}, slim)
		assertAbsent(t, argv, "--strict-mcp-config")
		// the other slim flags remain
		assertPairAfter(t, argv, "--thinking", "disabled")
	})
}

// JSONSchema emits the --json-schema pair profile-independently: for a full
// (zero slimArgv) request and a slim one alike.
func TestBuildArgv_JSONSchema(t *testing.T) {
	const bin, prof, model = "/v/claude", "/p/glm.json", "glm-4.6"
	const schema = `{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`

	t.Run("full", func(t *testing.T) {
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", JSONSchema: schema}, slimArgv{})
		assertPairAfter(t, argv, "--json-schema", schema)
	})

	t.Run("slim", func(t *testing.T) {
		slim := slimArgv{promptFile: "/abs/job.slimprompt", tools: []string{"Read"}}
		argv := buildArgv(bin, prof, model, Request{Prompt: "x", PromptProfile: ProfileSlim, JSONSchema: schema}, slim)
		assertPairAfter(t, argv, "--json-schema", schema)
	})
}

// ----- ResolveEffectiveProfile: pass-through + fail-open -----

func TestResolveEffectiveProfile(t *testing.T) {
	// The caller already loaded the recipe; ResolveEffectiveProfile resolves the version
	// against THAT fp, never a re-loaded one.
	fp := &fingerprint.Fingerprint{BinaryPath: "/v/claude"}

	// full / "" pass through unchanged with no resolution.
	for _, p := range []string{"", ProfileFull} {
		eff, dn := ResolveEffectiveProfile(p, fp)
		if eff != p || dn != "" {
			t.Fatalf("ResolveEffectiveProfile(%q) = (%q,%q), want (%q,\"\")", p, eff, dn, p)
		}
	}

	origVer := resolveBinaryPathVersion
	t.Cleanup(func() { resolveBinaryPathVersion = origVer })

	t.Run("at-floor keeps slim", func(t *testing.T) {
		resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
			return "/v/claude", SlimVersionFloor, nil
		}
		eff, dn := ResolveEffectiveProfile(ProfileSlim, fp)
		if eff != ProfileSlim || dn != "" {
			t.Fatalf("at-floor: got (%q,%q), want (slim,\"\")", eff, dn)
		}
	})

	t.Run("newer keeps slim-ro", func(t *testing.T) {
		resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
			return "/v/claude", "2.1.167", nil
		}
		eff, _ := ResolveEffectiveProfile(ProfileSlimRO, fp)
		if eff != ProfileSlimRO {
			t.Fatalf("newer: got %q, want slim-ro", eff)
		}
	})

	t.Run("below floor fails open to full", func(t *testing.T) {
		resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
			return "/v/claude", "2.1.50", nil
		}
		eff, dn := ResolveEffectiveProfile(ProfileSlim, fp)
		if eff != ProfileFull || !strings.Contains(dn, "2.1.50") || !strings.Contains(dn, SlimVersionFloor) {
			t.Fatalf("below floor: got (%q,%q), want full + reason naming the versions", eff, dn)
		}
	})

	t.Run("unknown version fails open to full", func(t *testing.T) {
		resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
			return "/v/claude", "", nil
		}
		eff, dn := ResolveEffectiveProfile(ProfileSlim, fp)
		if eff != ProfileFull || !strings.Contains(dn, "unknown") {
			t.Fatalf("unknown version: got (%q,%q), want full + 'unknown' reason", eff, dn)
		}
	})

	t.Run("resolve error fails open to full", func(t *testing.T) {
		resolveBinaryPathVersion = func(*fingerprint.Fingerprint) (string, string, error) {
			return "", "", fingerprint.ErrFingerprintStale
		}
		eff, dn := ResolveEffectiveProfile(ProfileSlim, fp)
		if eff != ProfileFull || dn == "" {
			t.Fatalf("resolve error: got (%q,%q), want full + non-empty reason", eff, dn)
		}
	})

	// The fp passed in is the one resolved against — a nil fp surfaces through the
	// resolver as a fail-open, never a panic or a second load.
	t.Run("nil fp fails open via the resolver", func(t *testing.T) {
		resolveBinaryPathVersion = func(f *fingerprint.Fingerprint) (string, string, error) {
			if f != nil {
				t.Fatalf("expected the caller-supplied fp, got %+v", f)
			}
			return "", "", fingerprint.ErrFingerprintStale
		}
		eff, dn := ResolveEffectiveProfile(ProfileSlim, nil)
		if eff != ProfileFull || dn == "" {
			t.Fatalf("nil fp: got (%q,%q), want full + non-empty reason", eff, dn)
		}
	})
}

// ----- validateSlimArgs: front-loaded rejections -----

func TestValidateSlimArgs(t *testing.T) {
	ok := func(req Request) {
		t.Helper()
		if r := validateSlimArgs(req); r != nil {
			t.Fatalf("validateSlimArgs(%+v) = %v, want nil", req, *r)
		}
	}
	bad := func(req Request) {
		t.Helper()
		r := validateSlimArgs(req)
		if r == nil || r.ErrorCode != ErrCodeBadArgs {
			t.Fatalf("validateSlimArgs(%+v) = %v, want SUBAGENT_BAD_ARGS", req, r)
		}
	}

	// Valid combos.
	ok(Request{})                                                              // full, no refinements
	ok(Request{PromptProfile: ProfileSlim})                                    // bare slim
	ok(Request{PromptProfile: ProfileSlim, NoSkills: true, MCP: true})         // slim + refinements
	ok(Request{PromptProfile: ProfileSlimRO, Tools: []string{"Read", "Grep"}}) // slim-ro + tools

	// Unknown profile.
	bad(Request{PromptProfile: "bogus"})
	// Refinements combined with full.
	bad(Request{Tools: []string{"Read"}})
	bad(Request{NoSkills: true})
	bad(Request{MCP: true})
	bad(Request{PromptProfile: ProfileFull, MCP: true})
	// Bad tool names under slim.
	bad(Request{PromptProfile: ProfileSlim, Tools: []string{"Read", "Nope"}})
	bad(Request{PromptProfile: ProfileSlim, Tools: []string{"Read", "Read"}})
	// Skill in an explicit set with skills disabled is contradictory.
	bad(Request{PromptProfile: ProfileSlim, Tools: []string{"Read", "Skill"}, NoSkills: true})
	// Skill with skills ON is fine; an explicit set without Skill under NoSkills is fine.
	ok(Request{PromptProfile: ProfileSlim, Tools: []string{"Read", "Skill"}})
	ok(Request{PromptProfile: ProfileSlim, Tools: []string{"Read", "Grep"}, NoSkills: true})
}

// ----- buildSlimArgv: tools override / skills toggle / sidecar write -----

func TestBuildSlimArgv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// full → no slim flags, no sidecar.
	sa, err := buildSlimArgv(ProfileFull, "job-1", Request{}, "m")
	if err != nil || sa.promptFile != "" {
		t.Fatalf("full buildSlimArgv = %+v, %v; want zero slimArgv", sa, err)
	}

	// The jobs dir exists by the time buildSlimArgv runs in production (the jobID
	// mint / launchBackground create it); mirror that here.
	dir, _ := jobsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("slim default tools + sidecar written 0600", func(t *testing.T) {
		sa, err := buildSlimArgv(ProfileSlim, "job-slim", Request{PromptProfile: ProfileSlim}, "vendor-m")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Bash", "Edit", "Glob", "Grep", "Read", "Skill", "Write"} // canonicalized (sorted)
		if !reflect.DeepEqual(sa.tools, want) {
			t.Fatalf("slim default tools = %v, want %v", sa.tools, want)
		}
		if sa.promptFile != filepath.Join(dir, "job-slim.slimprompt") {
			t.Fatalf("sidecar path = %q", sa.promptFile)
		}
		info, serr := os.Stat(sa.promptFile)
		if serr != nil {
			t.Fatalf("sidecar not written: %v", serr)
		}
		// NTFS reports 0666; the 0600 contract is unix-only.
		if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
			t.Fatalf("sidecar mode = %v, want 0600", got)
		}
		data, _ := os.ReadFile(sa.promptFile)
		if !strings.Contains(string(data), "You are powered by the model named vendor-m.") {
			t.Fatalf("rendered sidecar missing the model line")
		}
	})

	t.Run("skills off drops Skill", func(t *testing.T) {
		sa, err := buildSlimArgv(ProfileSlimRO, "job-ns", Request{PromptProfile: ProfileSlimRO, NoSkills: true}, "m")
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range sa.tools {
			if tool == "Skill" {
				t.Fatalf("NoSkills slim-ro must drop Skill: %v", sa.tools)
			}
		}
		want := []string{"Bash", "Glob", "Grep", "Read"}
		if !reflect.DeepEqual(sa.tools, want) {
			t.Fatalf("slim-ro NoSkills tools = %v, want %v", sa.tools, want)
		}
	})

	t.Run("explicit tools override the default set", func(t *testing.T) {
		sa, err := buildSlimArgv(ProfileSlim, "job-ovr",
			Request{PromptProfile: ProfileSlim, Tools: []string{"Grep", "Read"}}, "m")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"Grep", "Read"}
		if !reflect.DeepEqual(sa.tools, want) {
			t.Fatalf("override tools = %v, want %v", sa.tools, want)
		}
	})

	t.Run("empty jobID is an error for slim", func(t *testing.T) {
		if _, err := buildSlimArgv(ProfileSlim, "", Request{PromptProfile: ProfileSlim}, "m"); err == nil {
			t.Fatal("buildSlimArgv with empty jobID must error for a slim profile")
		}
	})
}

// ----- jobMeta roundtrip with the new profile fields -----

func TestJobMeta_ProfileFieldsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	in := jobMeta{
		JobID:         "job-rt",
		Vendor:        "glm",
		Model:         "glm-4.6",
		PromptProfile: ProfileSlimRO,
		SlimDowngrade: "slim disabled: claude version 2.1.50 below floor 2.1.88",
	}
	if err := writeMeta(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := readMeta(dir, "job-rt")
	if err != nil {
		t.Fatal(err)
	}
	if out.PromptProfile != in.PromptProfile || out.SlimDowngrade != in.SlimDowngrade {
		t.Fatalf("roundtrip lost profile fields: got %q/%q", out.PromptProfile, out.SlimDowngrade)
	}
}

// ----- removeJob cleans the .slimprompt sidecar -----

func TestRemoveJob_CleansSlimprompt(t *testing.T) {
	dir := t.TempDir()
	jobID := "job-clean"
	for _, ext := range []string{".json", ".out", ".err", ".slimprompt"} {
		if err := os.WriteFile(filepath.Join(dir, jobID+ext), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removeJob(dir, jobID)
	if _, err := os.Stat(filepath.Join(dir, jobID+".slimprompt")); !os.IsNotExist(err) {
		t.Fatalf(".slimprompt survived removeJob: %v", err)
	}
}
