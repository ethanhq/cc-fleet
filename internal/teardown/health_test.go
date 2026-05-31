package teardown

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyPaneOutput(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantStatus string
		wantClass  string
	}{
		{"clean", "● Running tests...\n  All green.", statusOK, ""},
		{"empty", "", statusOK, ""},
		// The real-world P1 line carries 429 + balance at once; balance wins.
		{"real-world 429+balance", "API Error: Request rejected (429) 余额不足或无可用资源包", statusError, errClassInsufficient},
		{"balance english", "Error: insufficient balance, please recharge", statusError, errClassInsufficient},
		{"quota exceeded", "You exceeded your current quota", statusError, errClassInsufficient},
		{"auth word", "API Error: Unauthorized", statusError, errClassAuth},
		{"auth invalid key", "Error: invalid api key provided", statusError, errClassAuth},
		{"auth paren 403", "request failed (403)", statusError, errClassAuth},
		{"rate limit phrase", "API Error: rate limit exceeded, retrying", statusError, errClassRateLimit},
		{"rate limit 429 paren", "got (429) from upstream", statusError, errClassRateLimit},
		{"generic api error", "API Error: something unexpected happened", statusError, errClassAPIError},
		{"overloaded", "Error: overloaded, please try again", statusError, errClassAPIError},
		{"case-insensitive", "API ERROR: RATE LIMIT", statusError, errClassRateLimit},
		// Priority: balance must beat auth when both signals appear.
		{"priority balance over auth", "(401) unauthorized; also 余额不足", statusError, errClassInsufficient},
		// Bare digits without parens / phrase must NOT trip a false positive.
		{"bare number no false positive", "compiled 429 files in 401ms", statusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, cl, detail := classifyPaneOutput(tc.in)
			if st != tc.wantStatus || cl != tc.wantClass {
				t.Fatalf("classify(%q) = (%q,%q), want (%q,%q)",
					tc.in, st, cl, tc.wantStatus, tc.wantClass)
			}
			// SECURITY: detail must never echo the raw pane text.
			if tc.in != "" && strings.Contains(detail, tc.in) {
				t.Fatalf("detail leaked raw pane text: %q", detail)
			}
		})
	}
}

// TestClassifyPaneOutput_NoKeyLeak proves the classifier returns only canonical
// strings: a key fragment present in the pane never appears in the detail.
func TestClassifyPaneOutput_NoKeyLeak(t *testing.T) {
	fakeKey := "sk-FAKEKEY1234567890abcdef"
	in := "API Error: Request rejected (429) 余额不足  Authorization: Bearer " + fakeKey
	st, cl, detail := classifyPaneOutput(in)
	if st != statusError || cl != errClassInsufficient {
		t.Fatalf("got (%q,%q), want (error,insufficient_balance)", st, cl)
	}
	if strings.Contains(detail, fakeKey) || strings.Contains(detail, "Bearer") {
		t.Fatalf("detail leaked key material: %q", detail)
	}
}

// TestAnnotateHealth_Seam drives the enrich path with an injected capture
// function so it needs no live tmux and touches no real pane. Covers the
// error, ok, and unknown (capture failed) branches.
func TestAnnotateHealth_Seam(t *testing.T) {
	orig := captureFn
	t.Cleanup(func() { captureFn = orig })

	captureFn = func(socket, pane string) (string, error) {
		switch pane {
		case "%1":
			return "API Error: Request rejected (429) 余额不足", nil
		case "%2":
			return "● Editing main.go ... done", nil
		default:
			return "", errors.New("can't find pane")
		}
	}

	got := AnnotateHealth([]Teammate{
		{Name: "a", PaneID: "%1"},
		{Name: "b", PaneID: "%2"},
		{Name: "c", PaneID: "%3"},
	})

	if got[0].Status != statusError || got[0].ErrorClass != errClassInsufficient {
		t.Fatalf("a: got status=%q class=%q", got[0].Status, got[0].ErrorClass)
	}
	// SECURITY: the enriched Detail is canonical and must never echo the raw
	// pane text fed by captureFn (which carried the vendor's "余额不足").
	if strings.Contains(got[0].Detail, "余额不足") {
		t.Fatalf("a: Detail leaked raw pane text: %q", got[0].Detail)
	}
	if got[0].Detail == "" {
		t.Fatalf("a: error row should carry a canonical Detail, got empty")
	}
	if got[1].Status != statusOK || got[1].ErrorClass != "" {
		t.Fatalf("b: got status=%q class=%q", got[1].Status, got[1].ErrorClass)
	}
	if got[2].Status != statusUnknown {
		t.Fatalf("c: got status=%q, want unknown", got[2].Status)
	}
	// unknown row must not claim an error class.
	if got[2].ErrorClass != "" {
		t.Fatalf("c: unknown row should have no error_class, got %q", got[2].ErrorClass)
	}
}

// TestCapturePane_FakeTmux exercises the real exec path against a fake tmux on
// PATH (same scheme discover_test.go uses), so we verify arg wiring without a
// live server.
func TestCapturePane_FakeTmux(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	// Echo a fixed banner so we can assert capturePane returns stdout verbatim.
	script := "#!/bin/sh\nprintf 'API Error: rate limit (429)\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	out, err := capturePane("", "%7")
	if err != nil {
		t.Fatalf("capturePane: %v", err)
	}
	if !strings.Contains(out, "rate limit (429)") {
		t.Fatalf("capturePane output = %q, want it to contain the banner", out)
	}
	if st, cl, _ := classifyPaneOutput(out); st != statusError || cl != errClassRateLimit {
		t.Fatalf("classify of captured = (%q,%q), want (error,rate_limit)", st, cl)
	}
}

// TestCapturePane_TmuxFails confirms a non-zero tmux exit (e.g. pane gone)
// surfaces as an error so AnnotateHealth can mark the row unknown.
func TestCapturePane_TmuxFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tmux")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if _, err := capturePane("", "%nope"); err == nil {
		t.Fatal("capturePane: want error on tmux exit 1, got nil")
	}
}
