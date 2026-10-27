package subagent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// Beyond Resolve and Prepare, codexEngine runs on the embedded default: every
// call that can fail returns a classified failure and none panics.
func TestCodexEngineSkeletonFailsClassified(t *testing.T) {
	var eng Engine = codexEngine{}
	spec := launchSpec{req: Request{Provider: "cx", PromptProfile: ProfileSlim}}

	if eff, dn := eng.GateProfile(ProfileSlim, "1.0.0"); eff != ProfileSlim || dn != "" {
		t.Errorf("GateProfile = (%q, %q), want the request back unchanged", eff, dn)
	}
	settings, failed := unimplementedEngine{}.Prepare(&config.Provider{Name: "cx"}, nil)
	if settings != "" || failed == nil || failed.OK || failed.ErrorCode != ErrCodeFailed || failed.Provider != "cx" {
		t.Errorf("Prepare = (%q, %+v), want a SUBAGENT_FAILED result", settings, failed)
	}
	if argv, sidecar, err := eng.Argv(spec); argv != nil || sidecar != "" || !errors.Is(err, errEngineUnimplemented) {
		t.Errorf("Argv = (%q, %q, %v), want errEngineUnimplemented", argv, sidecar, err)
	}
	if env := eng.Env([]string{"ANTHROPIC_API_KEY=host"}, spec); env == nil || len(env) != 0 {
		t.Errorf("Env = %#v, want empty and non-nil (nil inherits the host env)", env)
	}
	if res := eng.Classify(spec, []byte("out"), nil, 0, false); res.OK || res.ErrorCode != ErrCodeFailed || res.Provider != "cx" {
		t.Errorf("Classify = %+v, want a SUBAGENT_FAILED result", res)
	}
}

// stubProviderProxy replaces ensureProviderProxy for the test and counts calls;
// check runs inside each call before it returns err.
func stubProviderProxy(t *testing.T, err error, check func()) *int {
	t.Helper()
	calls := 0
	orig := ensureProviderProxy
	ensureProviderProxy = func(*config.Provider, *diag.Logger) error {
		calls++
		if check != nil {
			check()
		}
		return err
	}
	t.Cleanup(func() { ensureProviderProxy = orig })
	return &calls
}

func TestCodexPrepareWritesHome(t *testing.T) {
	for name, v := range map[string]*config.Provider{
		"openai-responses": {Name: "cx", Protocol: config.ProtocolOpenAIResponses,
			BaseURL: "http://127.0.0.1:17223/", UpstreamURL: "https://api.example.com/v1"},
		// A codex row predating the protocol field is daemon-backed by its backend.
		"legacy codex-oauth": {Name: "cx", SecretBackend: config.CodexOAuthBackend,
			SecretRef: "codex-oauth", BaseURL: "http://127.0.0.1:17222/"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			want, err := profile.CodexHomePath("cx")
			if err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(want, "config.toml")
			calls := stubProviderProxy(t, nil, func() {
				if _, err := os.Stat(cfg); !os.IsNotExist(err) {
					t.Errorf("config.toml exists before the daemon is ensured (stat err=%v)", err)
				}
			})

			home, failed := codexEngine{}.Prepare(v, nil)
			if failed != nil {
				t.Fatalf("Prepare failed: %+v", failed)
			}
			if home != want {
				t.Errorf("home = %q, want %q", home, want)
			}
			if _, err := os.Stat(cfg); err != nil {
				t.Errorf("config.toml not written: %v", err)
			}
			if *calls != 1 {
				t.Errorf("ensureProviderProxy calls = %d, want 1", *calls)
			}
		})
	}
}

func TestCodexPrepareProxyFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stubProviderProxy(t, errors.New("codex proxy did not become ready on port 17223"), nil)
	v := &config.Provider{Name: "cx", Protocol: config.ProtocolOpenAIResponses,
		BaseURL: "http://127.0.0.1:17223/", UpstreamURL: "https://api.example.com/v1"}

	home, failed := codexEngine{}.Prepare(v, nil)
	if home != "" || failed == nil || failed.ErrorCode != ErrCodeProxyUnavailable || failed.Provider != "cx" {
		t.Fatalf("Prepare = (%q, %+v), want CODEX_PROXY_UNAVAILABLE", home, failed)
	}
	dir, _ := profile.CodexHomePath("cx")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("codex home must not be written on daemon failure (stat err=%v)", err)
	}
}

func TestCodexPrepareWriteFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker)
	stubProviderProxy(t, nil, nil)
	v := &config.Provider{Name: "cx", Protocol: config.ProtocolOpenAIResponses,
		BaseURL: "http://127.0.0.1:17223/", UpstreamURL: "https://api.example.com/v1"}

	home, failed := codexEngine{}.Prepare(v, nil)
	if home != "" || failed == nil || failed.ErrorCode != ErrCodeFailed || failed.Provider != "cx" {
		t.Fatalf("Prepare = (%q, %+v), want SUBAGENT_FAILED", home, failed)
	}
}

func TestCodexPrepareRejectsDirectRow(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	calls := stubProviderProxy(t, nil, nil)
	v := &config.Provider{Name: "ds", BaseURL: "https://api.deepseek.com/anthropic", SecretBackend: "file"}

	home, failed := codexEngine{}.Prepare(v, nil)
	if home != "" || failed == nil || failed.ErrorCode != ErrCodeFailed || failed.Provider != "ds" {
		t.Fatalf("Prepare = (%q, %+v), want SUBAGENT_FAILED", home, failed)
	}
	if *calls != 0 {
		t.Errorf("ensureProviderProxy calls = %d, want 0 for a direct row", *calls)
	}
	dir, _ := profile.CodexHomePath("ds")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("codex home must not be written for a direct row (stat err=%v)", err)
	}
}
