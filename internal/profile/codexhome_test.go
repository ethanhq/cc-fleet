package profile

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/ethanhq/cc-fleet/internal/config"
)

func sampleCodexProvider() *config.Provider {
	return &config.Provider{
		Name:          "cx",
		Protocol:      config.ProtocolOpenAIResponses,
		BaseURL:       "http://localhost:17223/",
		UpstreamURL:   "https://api.example.com/v1",
		DefaultModel:  "gpt-test",
		SecretBackend: "file",
		SecretRef:     "cx.key",
		Enabled:       true,
	}
}

// isolateConfigDir points config.ConfigDir at a temp dir and returns it.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	return xdg
}

const codexConfigSnapshot = `model_provider = "ccfleet"

[model_providers]
  [model_providers.ccfleet]
    name = "cc-fleet cx"
    base_url = "http://127.0.0.1:17223/v1"
    wire_api = "responses"
    [model_providers.ccfleet.auth]
      command = "/usr/local/bin/cc-fleet"
      args = ["keyget", "cx"]
      timeout_ms = 30000
      refresh_interval_ms = 300000

[history]
  persistence = "none"
`

func TestGenerateCodexConfig_Snapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pins a unix helper path; TestGenerateCodexConfig_RoundTrip covers windows")
	}
	got, err := GenerateCodexConfig(sampleCodexProvider(), testHelperBin())
	if err != nil {
		t.Fatalf("GenerateCodexConfig: %v", err)
	}
	if string(got) != codexConfigSnapshot {
		t.Fatalf("config.toml mismatch\n got:\n%s\nwant:\n%s", got, codexConfigSnapshot)
	}
}

func TestGenerateCodexConfig_RoundTrip(t *testing.T) {
	got, err := GenerateCodexConfig(sampleCodexProvider(), testHelperBin())
	if err != nil {
		t.Fatalf("GenerateCodexConfig: %v", err)
	}
	var back codexConfig
	if _, err := toml.Decode(string(got), &back); err != nil {
		t.Fatalf("Decode: %v\n%s", err, got)
	}
	p := back.ModelProviders.CCFleet
	if back.ModelProvider != "ccfleet" || p.WireAPI != "responses" || back.History.Persistence != "none" {
		t.Errorf("provider wiring = %+v", back)
	}
	if p.Auth.Command != testHelperBin() {
		t.Errorf("auth.command = %q, want %q", p.Auth.Command, testHelperBin())
	}
	if len(p.Auth.Args) != 2 || p.Auth.Args[0] != "keyget" || p.Auth.Args[1] != "cx" {
		t.Errorf("auth.args = %q, want [keyget cx]", p.Auth.Args)
	}
	if p.Auth.TimeoutMS != codexAuthTimeoutMS || p.Auth.RefreshIntervalMS != codexAuthRefreshMS {
		t.Errorf("auth timing = %d/%d", p.Auth.TimeoutMS, p.Auth.RefreshIntervalMS)
	}
	if p.BaseURL != "http://127.0.0.1:17223/v1" {
		t.Errorf("base_url = %q", p.BaseURL)
	}
}

func TestGenerateCodexConfig_BaseURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"http://127.0.0.1:17222/", "http://127.0.0.1:17222/v1"},
		{"http://127.0.0.1:17222", "http://127.0.0.1:17222/v1"},
		{"http://localhost:17222/v1", "http://127.0.0.1:17222/v1"},
		{"http://127.0.0.1/", ""},
		{"https://api.example.com:443/", ""},
	} {
		v := sampleCodexProvider()
		v.BaseURL = tc.in
		got, err := GenerateCodexConfig(v, testHelperBin())
		if tc.want == "" {
			if err == nil {
				t.Errorf("%s: want error, got\n%s", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		var back codexConfig
		if _, err := toml.Decode(string(got), &back); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if back.ModelProviders.CCFleet.BaseURL != tc.want {
			t.Errorf("%s: base_url = %q, want %q", tc.in, back.ModelProviders.CCFleet.BaseURL, tc.want)
		}
	}
}

func TestGenerateCodexConfig_Rejects(t *testing.T) {
	if _, err := GenerateCodexConfig(nil, testHelperBin()); err == nil {
		t.Error("nil provider: want error")
	}
	for name, mut := range map[string]func(*config.Provider, *string){
		"empty name":      func(v *config.Provider, _ *string) { v.Name = "" },
		"bad name":        func(v *config.Provider, _ *string) { v.Name = "../x" },
		"empty base_url":  func(v *config.Provider, _ *string) { v.BaseURL = "" },
		"empty helper":    func(_ *config.Provider, h *string) { *h = "" },
		"relative helper": func(_ *config.Provider, h *string) { *h = "bin/cc-fleet" },
	} {
		v, helper := sampleCodexProvider(), testHelperBin()
		mut(v, &helper)
		if got, err := GenerateCodexConfig(v, helper); err == nil {
			t.Errorf("%s: want error, got\n%s", name, got)
		}
	}
}

func TestWriteCodexHome(t *testing.T) {
	xdg := isolateConfigDir(t)
	v := sampleCodexProvider()

	dir, err := WriteCodexHome(v, testHelperBin())
	if err != nil {
		t.Fatalf("first WriteCodexHome: %v", err)
	}
	want, _ := CodexHomePath("cx")
	if dir != want || dir != filepath.Join(xdg, "cc-fleet", "codex-home", "cx") {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	path := filepath.Join(dir, "config.toml")
	if runtime.GOOS != "windows" {
		for p, mode := range map[string]os.FileMode{dir: 0o700, filepath.Dir(dir): 0o700, path: 0o600} {
			info, err := os.Stat(p)
			if err != nil {
				t.Fatalf("Stat %s: %v", p, err)
			}
			if got := info.Mode().Perm(); got != mode {
				t.Errorf("%s mode = %o, want %o", p, got, mode)
			}
		}
	}

	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if _, err := WriteCodexHome(v, testHelperBin()); err != nil {
		t.Fatalf("second WriteCodexHome: %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("unchanged config was rewritten (stat err=%v)", err)
	}

	v.BaseURL = "http://127.0.0.1:17224/"
	if _, err := WriteCodexHome(v, testHelperBin()); err != nil {
		t.Fatalf("third WriteCodexHome: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(got, []byte(`base_url = "http://127.0.0.1:17224/v1"`)) {
		t.Fatalf("changed config was not rewritten:\n%s", got)
	}
}

func TestWriteCodexHomeConcurrent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("concurrent replaces can exhaust windows' rename retry budget")
	}
	isolateConfigDir(t)
	v := sampleCodexProvider()
	want, err := GenerateCodexConfig(v, testHelperBin())
	if err != nil {
		t.Fatalf("GenerateCodexConfig: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := WriteCodexHome(v, testHelperBin())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("WriteCodexHome: %v", err)
		}
	}
	dir, _ := CodexHomePath("cx")
	got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("config.toml = %q (err %v), want %q", got, err, want)
	}
}

func TestWriteCodexHomeConfigDirIsFile(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", blocker)
	if _, err := WriteCodexHome(sampleCodexProvider(), testHelperBin()); err == nil {
		t.Fatal("want error when the config dir is a file")
	}
}

func TestCodexHomePath(t *testing.T) {
	isolateConfigDir(t)
	for _, name := range []string{"", "../x", "a/b"} {
		if p, err := CodexHomePath(name); err == nil {
			t.Errorf("CodexHomePath(%q) = %q, want error", name, p)
		}
	}

	// A relative XDG_CONFIG_HOME still yields an absolute CODEX_HOME.
	t.Chdir(t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "rel")
	p, err := CodexHomePath("cx")
	if err != nil {
		t.Fatalf("CodexHomePath: %v", err)
	}
	if !filepath.IsAbs(p) {
		t.Fatalf("CodexHomePath = %q, want absolute", p)
	}
}
