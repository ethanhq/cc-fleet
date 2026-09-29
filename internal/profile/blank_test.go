package profile

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// secGenerateEnv renders the sample provider's profile and returns its env map.
func secGenerateEnv(t *testing.T) map[string]string {
	t.Helper()
	got, err := GenerateForProvider(sampleProvider(), testHelperBin())
	if err != nil {
		t.Fatalf("GenerateForProvider: %v", err)
	}
	var back struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return back.Env
}

// secAssertBlank fails unless every key is present in env with the value "".
func secAssertBlank(t *testing.T, env map[string]string, keys []string) {
	t.Helper()
	for _, k := range keys {
		v, ok := env[k]
		if !ok {
			t.Errorf("env[%s] missing, want present and blank", k)
			continue
		}
		if v != "" {
			t.Errorf("env[%s] = %q, want \"\"", k, v)
		}
	}
}

// TestGenerateBlanksLeadCredentials: the profile's settings layer overrides the
// lead's credentials and custom headers with blanks, so only the apiKeyHelper
// key reaches the provider.
func TestGenerateBlanksLeadCredentials(t *testing.T) {
	secAssertBlank(t, secGenerateEnv(t), []string{
		"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS",
	})
}

// TestGenerateBlanksBackendSelectors: every cloud-backend switch is blanked so a
// lead's Bedrock/Vertex/etc. setting cannot reroute the provider request.
func TestGenerateBlanksBackendSelectors(t *testing.T) {
	secAssertBlank(t, secGenerateEnv(t), []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
		"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
	})
}

// TestGenerateKeepsAgentTeams: native teammates need the agent-teams switch, so
// the profile must not touch it; the base URL still comes from the provider.
func TestGenerateKeepsAgentTeams(t *testing.T) {
	env := secGenerateEnv(t)
	if v, ok := env["CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS"]; ok {
		t.Fatalf("profile sets CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS = %q, want absent", v)
	}
	if got, want := env["ANTHROPIC_BASE_URL"], sampleProvider().BaseURL; got != want {
		t.Fatalf("env[ANTHROPIC_BASE_URL] = %q, want %q", got, want)
	}
}

// TestWriteForProviderSkipsUnchanged: rewriting identical bytes leaves the file
// (and its mtime) untouched; a content change is still written.
func TestWriteForProviderSkipsUnchanged(t *testing.T) {
	isolateHome(t)
	v := sampleProvider()

	path, err := WriteForProvider(v, testHelperBin())
	if err != nil {
		t.Fatalf("first WriteForProvider: %v", err)
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if _, err := WriteForProvider(v, testHelperBin()); err != nil {
		t.Fatalf("second WriteForProvider: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("unchanged profile was rewritten: mtime = %v, want %v", info.ModTime(), old)
	}

	v.DefaultModel = "deepseek-v4-pro"
	if _, err := WriteForProvider(v, testHelperBin()); err != nil {
		t.Fatalf("third WriteForProvider: %v", err)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.ModTime().Equal(old) {
		t.Fatalf("changed profile was not rewritten (mtime still %v)", old)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want, err := GenerateForProvider(v, testHelperBin())
	if err != nil {
		t.Fatalf("GenerateForProvider: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("file content mismatch after change:\n--- file ---\n%s\n--- want ---\n%s", got, want)
	}
}
