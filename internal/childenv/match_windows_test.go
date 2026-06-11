//go:build windows

package childenv

import (
	"strings"
	"testing"
)

// TestClean_DropsMixedCaseOnWindows: Windows env names are case-insensitive, so a
// child that sets a lowercase or mixed-case credential name addresses the same
// variable and must still be scrubbed.
func TestClean_DropsMixedCaseOnWindows(t *testing.T) {
	in := []string{
		"anthropic_api_key=sk-leak",
		"Anthropic_Auth_Token=tok-leak",
		"ClaudeCode=1",
		"PATH=C:\\Windows",
	}
	out := Clean(in)

	joined := strings.Join(out, "\n")
	for _, secret := range []string{"sk-leak", "tok-leak"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("Clean leaked %q on windows: %q", secret, joined)
		}
	}
	for _, banned := range []string{"anthropic_api_key", "Anthropic_Auth_Token", "ClaudeCode"} {
		for _, kv := range out {
			if strings.HasPrefix(kv, banned+"=") {
				t.Fatalf("Clean kept mixed-case %q: %q", banned, kv)
			}
		}
	}
	if !containsLine(out, "PATH=C:\\Windows") {
		t.Fatalf("Clean dropped a keeper var on windows: %v", out)
	}
}
