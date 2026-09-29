//go:build windows

package teammate

import (
	"bytes"
	"strings"
	"testing"
)

func TestLaunchWindows(t *testing.T) {
	hermeticHome(t)
	execs := 0
	oldExec := execFn
	t.Cleanup(func() { execFn = oldExec })
	execFn = func(string, []string, []string) error { execs++; return nil }

	for _, args := range [][]string{
		{"--shim-protocol", "1"},
		{"--agent-id", "w@session-1", "--agent-type", "ccf-p"},
		{"--agent-id=w@session-1", "--agent-type", "general-purpose"},
	} {
		var out, errb bytes.Buffer
		code := launchMain(args, &out, &errb)
		if code != 1 || out.Len() != 0 {
			t.Errorf("%q = (%d, %q), want (1, \"\")", args, code, out.String())
		}
		line := errb.String()
		if strings.Count(line, "\n") != 1 {
			t.Fatalf("%q stderr = %q, want one line", args, line)
		}
		wantID := "-"
		if len(args) > 2 {
			wantID = "w@session-1"
		}
		if c, id, ok := ParseFailureLine(strings.TrimSuffix(line, "\n")); !ok || c != CodeUnsupportedOnWindows || id != wantID {
			t.Errorf("%q failure line %q parsed as (%q, %q, %v)", args, line, c, id, ok)
		}
	}
	if execs != 0 {
		t.Errorf("exec'd %d times on Windows", execs)
	}
}
