//go:build darwin

package procintrospect

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// The darwin readers recover a process's argv by space-splitting `ps -o
// command=` output, since macOS exposes no NUL-delimited argv without cgo. The
// existing real-child test only covers `sleep <n>` — two whitespace-free
// tokens. Reap (Cmdline), discovery (ProcessTable), and permission inheritance
// all match cc-fleet markers that sit in a long multi-flag teammate command
// line, so these tests inject a realistic synthetic command line through the
// execCommand seam (NO real ps, NO real teammate process) and assert the
// markers survive the split adjacent to their values — plus a variant that
// documents the known lossy degradation when an argument itself contains a
// space.

const (
	fakeChildEnv  = "CCF_PROCINTROSPECT_FAKE_CHILD"
	fakeOutputEnv = "CCF_PROCINTROSPECT_FAKE_OUTPUT"
)

// TestHelperProcess is not a real test. When the fake-child env is set it acts
// as a stand-in for ps(1)/pgrep(1): it prints the canned output supplied via
// the env and exits, so the darwin readers can be exercised with fully
// synthetic input. os.Exit before the test framework runs keeps stdout clean
// (no trailing "PASS"), so .Output() returns exactly the canned bytes.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(fakeChildEnv) != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv(fakeOutputEnv))
	os.Exit(0)
}

// stubExecCommand redirects the package execCommand seam at TestHelperProcess,
// which echoes output on stdout. The original is restored via t.Cleanup. Only
// Cmdline/ProcessTable are stub-able this way — they call .Output() directly;
// ProcStart resets cmd.Env and would drop the fake-child env.
func stubExecCommand(t *testing.T, output string) {
	t.Helper()
	orig := execCommand
	t.Cleanup(func() { execCommand = orig })
	execCommand = func(_ string, _ ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), fakeChildEnv+"=1", fakeOutputEnv+"="+output)
		return cmd
	}
}

// valueAfter returns the token immediately following flag in argv, proving the
// flag and its value stayed adjacent across the split.
func valueAfter(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag {
			if i+1 < len(argv) {
				return argv[i+1], true
			}
			return "", false
		}
	}
	return "", false
}

// A realistic teammate command line: claude binary + the full flag set cc-fleet
// (and native CC) pass when spawning a teammate. None of these values contains
// whitespace, so the space-split must recover every one.
const fakeTeammateCmd = "/root/.local/share/claude/bin/claude --agent-id w@t --agent-name w " +
	"--team-name t --agent-color blue --agent-type general-purpose " +
	"--settings /root/.cc-fleet/profiles/deepseek.json --model deepseek-chat\n"

// TestCmdline_DarwinRecoversMultiFlagMarkers verifies the per-pid reader (used
// by the spawn rollback reap + permission inheritance) recovers each cc-fleet
// marker adjacent to its value out of a long multi-flag command line.
func TestCmdline_DarwinRecoversMultiFlagMarkers(t *testing.T) {
	stubExecCommand(t, fakeTeammateCmd)

	argv, err := Cmdline(4321)
	if err != nil {
		t.Fatalf("Cmdline: %v", err)
	}

	want := map[string]string{
		"--agent-id":   "w@t",
		"--team-name":  "t",
		"--agent-type": "general-purpose",
	}
	for flag, val := range want {
		got, ok := valueAfter(argv, flag)
		if !ok {
			t.Errorf("marker %q not found in split argv %v", flag, argv)
			continue
		}
		if got != val {
			t.Errorf("value after %q = %q, want %q (split lost flag→value adjacency)", flag, got, val)
		}
	}
}

// TestProcessTable_DarwinRecoversMultiFlagMarkers verifies the whole-table scan
// (used by board / hide-show discovery + ghost reap) strips the leading pid and
// still recovers the markers adjacent to their values.
func TestProcessTable_DarwinRecoversMultiFlagMarkers(t *testing.T) {
	// ps -axww -o pid=,command= renders " <pid> <command>"; mimic that shape.
	stubExecCommand(t, "  4321 "+fakeTeammateCmd)

	procs, err := ProcessTable()
	if err != nil {
		t.Fatalf("ProcessTable: %v", err)
	}
	var argv []string
	for _, p := range procs {
		if p.PID == 4321 {
			argv = p.Argv
			break
		}
	}
	if argv == nil {
		t.Fatalf("ProcessTable did not surface pid 4321; procs = %v", procs)
	}
	for flag, val := range map[string]string{
		"--agent-id":   "w@t",
		"--team-name":  "t",
		"--agent-type": "general-purpose",
	} {
		got, ok := valueAfter(argv, flag)
		if !ok || got != val {
			t.Errorf("value after %q = (%q,%v), want %q (pid-strip or split corrupted argv %v)", flag, got, ok, val, argv)
		}
	}
}

// TestCmdline_DarwinSpaceInSettingsPathDegrades documents the known limitation:
// an argument that ITSELF contains a space (e.g. a --settings path under a HOME
// with a space) cannot be perfectly reconstructed by the space-split. The
// degradation is LOCAL — markers before the spaced argument still recover —
// while the spaced --settings value is truncated at the first space. This is
// acceptable because every cc-fleet marker we match on is whitespace-free.
func TestCmdline_DarwinSpaceInSettingsPathDegrades(t *testing.T) {
	// --settings path contains a space ("jo bloggs"); --model trails it.
	const spaced = "/root/.local/share/claude/bin/claude --agent-id w@t --team-name t " +
		"--agent-type general-purpose --settings /root/jo bloggs/.claude/x.json --model m\n"
	stubExecCommand(t, spaced)

	argv, err := Cmdline(4321)
	if err != nil {
		t.Fatalf("Cmdline: %v", err)
	}

	// Markers BEFORE the spaced argument are unaffected — local degradation.
	for flag, val := range map[string]string{
		"--agent-id":   "w@t",
		"--team-name":  "t",
		"--agent-type": "general-purpose",
	} {
		if got, ok := valueAfter(argv, flag); !ok || got != val {
			t.Errorf("pre-space marker %q = (%q,%v), want %q (degradation should be local, not global)", flag, got, ok, val)
		}
	}

	// The spaced --settings value is truncated at the first space: recovery
	// yields only the leading fragment, NOT the full path. This asserts the
	// documented lossy behavior so a future "fix" that changes it is noticed.
	got, ok := valueAfter(argv, "--settings")
	if !ok {
		t.Fatalf("--settings marker missing from %v", argv)
	}
	if got != "/root/jo" {
		t.Fatalf("value after --settings = %q, want truncated %q (space-split is lossy here)", got, "/root/jo")
	}
	for _, a := range argv {
		if a == "/root/jo bloggs/.claude/x.json" {
			t.Fatalf("space-split unexpectedly recovered the full spaced path as one token: %v", argv)
		}
	}
}
