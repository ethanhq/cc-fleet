package teammate

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// defsSh is the real POSIX shell the shim tests execute rendered shims with.
const defsSh = "/bin/sh"

// defsArgsDump is a sh fragment that prints its tag then every argument as [arg].
const defsArgsDump = `for a in "$@"; do printf ' [%s]' "$a"; done; echo`

// defsFakeLauncher writes a fake cc-fleet at path: it answers the shim probe
// (__teammate-launch --shim-protocol 1) with exit probeExit, logs every call to
// <path>.log, and otherwise prints "LAUNCH [arg]...".
func defsFakeLauncher(t *testing.T, path string, probeExit int) string {
	t.Helper()
	body := `printf '%s\n' "$*" >> "$0.log"
if [ "$1" = "__teammate-launch" ] && [ "$2" = "--shim-protocol" ]; then
  echo 1
  exit ` + map[bool]string{true: "0", false: "1"}[probeExit == 0] + `
fi
printf LAUNCH; ` + defsArgsDump + "\n"
	return writeScript(t, path, body)
}

// defsFakeClaude writes a fake claude at dir/claude that prints "CLAUDE [arg]..."
// and touches <dir>/claude.ran.
func defsFakeClaude(t *testing.T, dir string) string {
	t.Helper()
	return writeFakeClaude(t, dir, `: > "$0.ran"
printf CLAUDE; `+defsArgsDump+"\n")
}

// defsRunShim renders a shim pinned to ccf and runs it with the real /bin/sh.
func defsRunShim(t *testing.T, ccf string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a POSIX sh script")
	}
	shim := filepath.Join(t.TempDir(), ShimFileName)
	if err := os.WriteFile(shim, RenderShim(ccf), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Logf("exec %s %s %q", defsSh, shim, args)
	cmd := exec.Command(defsSh, append([]string{shim}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run shim: %v", err)
	}
	return out.String(), errb.String(), code
}

func TestRenderShimMatchesDesign(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "defs", "shim.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")) // a Windows checkout may convert line endings
	got := RenderShim("/opt/cc fleet/it's/cc-fleet")
	if !bytes.Equal(got, want) {
		t.Fatalf("RenderShim differs from the design template:\n--- got\n%s\n--- want\n%s", got, want)
	}
	lines := strings.Split(string(got), "\n")
	if !strings.Contains(lines[1], ShimMarker) {
		t.Fatalf("line 2 %q lacks the marker %q", lines[1], ShimMarker)
	}
}

func TestShimProbeExecsLauncher(t *testing.T) {
	h := hermeticHome(t)
	defsFakeClaude(t, h.Bin)
	ccf := defsFakeLauncher(t, filepath.Join(t.TempDir(), "cc-fleet"), 0)
	args := []string{"--agent-id", "w one@session-abcd1234", "--agent-type", "ccf-glm", "--model", "x", ""}
	out, errOut, code := defsRunShim(t, ccf, args...)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := "LAUNCH [__teammate-launch] [--agent-id] [w one@session-abcd1234] [--agent-type] [ccf-glm] [--model] [x] []\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	log, err := os.ReadFile(ccf + ".log")
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSuffix(string(log), "\n"), "\n")
	if len(calls) != 2 || calls[0] != "__teammate-launch --shim-protocol 1" {
		t.Fatalf("launcher calls = %q, want the probe then the exec", calls)
	}
	if _, err := os.Stat(filepath.Join(h.Bin, "claude.ran")); err == nil {
		t.Fatal("claude must not run when the launcher takes over")
	}
}

func TestShimFallbackNativeExecsClaude(t *testing.T) {
	native := []string{"--agent-id", "helper@session-abcd1234", "--agent-type", "general-purpose", "--x", "a b"}
	nativeOut := "CLAUDE [--agent-id] [helper@session-abcd1234] [--agent-type] [general-purpose] [--x] [a b]\n"
	cases := []struct {
		name      string
		launcher  func(t *testing.T) string
		args      []string
		want      string
		homeLocal bool // claude only at $HOME/.local/bin/claude, not on PATH
	}{
		{"launcher missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, native, nativeOut, false},
		{"old launcher fails the probe", func(t *testing.T) string {
			return defsFakeLauncher(t, filepath.Join(t.TempDir(), "cc-fleet"), 1)
		}, native, nativeOut, false},
		{"no agent type", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			[]string{"--agent-id", "x@t"}, "CLAUDE [--agent-id] [x@t]\n", false},
		{"claude from HOME/.local/bin", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, native, nativeOut, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hermeticHome(t)
			if tc.homeLocal {
				defsFakeClaude(t, filepath.Join(h.Home, ".local", "bin"))
				if p, err := exec.LookPath("claude"); err == nil {
					t.Fatalf("a claude is on the hermetic PATH: %s", p)
				}
			} else {
				defsFakeClaude(t, h.Bin)
			}
			out, errOut, code := defsRunShim(t, tc.launcher(t), tc.args...)
			if code != 0 || errOut != "" || out != tc.want {
				t.Fatalf("exit %d, stdout %q, stderr %q; want stdout %q", code, out, errOut, tc.want)
			}
		})
	}
}

func TestShimFallbackCcfFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		launcher func(t *testing.T) string
		args     []string
	}{
		{"separate flags, launcher missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			[]string{"--agent-id", "w@session-abcd1234", "--agent-type", "ccf-glm"}},
		{"= flags, launcher missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			[]string{"--agent-id=w@session-abcd1234", "--agent-type=ccf-glm.strong"}},
		{"separate flags, old launcher", func(t *testing.T) string {
			return defsFakeLauncher(t, filepath.Join(t.TempDir(), "cc-fleet"), 1)
		}, []string{"--agent-type", "ccf-glm.fast", "--agent-id", "w@session-abcd1234"}},
		{"= flags, old launcher", func(t *testing.T) string {
			return defsFakeLauncher(t, filepath.Join(t.TempDir(), "cc-fleet"), 1)
		}, []string{"--agent-type=ccf-glm", "--agent-id=w@session-abcd1234"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hermeticHome(t)
			defsFakeClaude(t, h.Bin)
			ccf := tc.launcher(t)
			out, errOut, code := defsRunShim(t, ccf, tc.args...)
			if code != 1 || out != "" {
				t.Fatalf("exit %d, stdout %q; want exit 1 and no stdout", code, out)
			}
			lines := strings.Split(strings.TrimSuffix(errOut, "\n"), "\n")
			if len(lines) != 1 {
				t.Fatalf("stderr must be one line, got %q", errOut)
			}
			gotCode, agentID, ok := ParseFailureLine(lines[0])
			if !ok || gotCode != CodeSetupRequired || agentID != "w@session-abcd1234" {
				t.Fatalf("failure line %q parsed as (%q, %q, %v)", lines[0], gotCode, agentID, ok)
			}
			if !strings.Contains(lines[0], DetailLauncherTargetMissing+" ("+ccf+")") {
				t.Fatalf("failure line %q lacks %s and the pinned path", lines[0], DetailLauncherTargetMissing)
			}
			if _, err := os.Stat(filepath.Join(h.Bin, "claude.ran")); err == nil {
				t.Fatal("claude ran for a ccf-* teammate")
			}
		})
	}
}

func TestShimQuotesPathWithSpaceAndQuote(t *testing.T) {
	h := hermeticHome(t)
	defsFakeClaude(t, h.Bin)
	dir := filepath.Join(t.TempDir(), `it's a "dir" $HOME `+"`x`")
	ccf := defsFakeLauncher(t, filepath.Join(dir, "cc fleet"), 0)
	out, errOut, code := defsRunShim(t, ccf, "--agent-type", "ccf-glm")
	if code != 0 || errOut != "" || out != "LAUNCH [__teammate-launch] [--agent-type] [ccf-glm]\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if got, ok := parsePinned(RenderShim(ccf)); !ok || got != ccf {
		t.Fatalf("parsePinned = %q, %v; want %q", got, ok, ccf)
	}
	for _, s := range []string{"", "plain", "a'b", "''", "'", `\'`, "a b'c'd"} {
		if got, ok := shUnquote(shQuote(s)); !ok || got != s {
			t.Errorf("shUnquote(shQuote(%q)) = %q, %v", s, got, ok)
		}
	}
}

func TestInspectShim(t *testing.T) {
	h := hermeticHome(t)
	ccf := defsFakeLauncher(t, filepath.Join(h.Home, "cc-fleet"), 0)

	missing := InspectShim(filepath.Join(h.Home, "nope"))
	if missing.Exists || missing.Managed || missing.Executable || missing.Pinned != "" {
		t.Fatalf("missing shim: %+v", missing)
	}

	path, _, err := WriteShim(ccf)
	if err != nil {
		t.Fatal(err)
	}
	st := InspectShim(path)
	want := ShimStatus{Path: path, Exists: true, Managed: true, Executable: true, Pinned: ccf, PinnedExists: true}
	if st != want {
		t.Fatalf("InspectShim = %+v, want %+v", st, want)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if st := InspectShim(path); st.Executable || !st.Managed {
		t.Fatalf("0644 shim: %+v", st)
	}

	if err := os.Remove(ccf); err != nil {
		t.Fatal(err)
	}
	if st := InspectShim(path); st.Pinned != ccf || st.PinnedExists || st.PinnedIsSelf {
		t.Fatalf("pinned binary removed: %+v", st)
	}

	self, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteShim(""); err != nil {
		t.Fatal(err)
	}
	if st := InspectShim(path); st.Pinned != self || !st.PinnedExists || !st.PinnedIsSelf || !st.Executable {
		t.Fatalf("WriteShim(\"\") should pin the running binary %q: %+v", self, st)
	}

	foreign := writeScript(t, filepath.Join(h.Home, "foreign"), "ccf='/bin/true'\nexec claude \"$@\"\n")
	if st := InspectShim(foreign); !st.Exists || st.Managed || !st.Executable || st.Pinned != "/bin/true" {
		t.Fatalf("foreign script: %+v", st)
	}
}

func TestIsOurShim(t *testing.T) {
	h := hermeticHome(t)
	path, err := ShimPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(h.ConfigHome, "cc-fleet", "bin", ShimFileName); path != want {
		t.Fatalf("ShimPath = %q, want %q", path, want)
	}
	if !IsOurShim(path) {
		t.Fatal("ShimPath() itself is ours even before it is written")
	}
	copyPath := filepath.Join(h.Home, "elsewhere", "claude-teammate")
	if err := os.MkdirAll(filepath.Dir(copyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, RenderShim("/x/cc-fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !IsOurShim(copyPath) {
		t.Fatal("a file carrying the shim marker is ours")
	}
	foreign := writeScript(t, filepath.Join(h.Home, "wrapper"), "exec claude \"$@\"\n")
	for _, v := range []string{"", foreign, filepath.Join(h.Home, "missing"), h.Home} {
		if IsOurShim(v) {
			t.Errorf("IsOurShim(%q) = true", v)
		}
	}
}

func TestWriteShimRestoresMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("asserts POSIX file modes")
	}
	h := hermeticHome(t)
	ccf := filepath.Join(h.Home, "cc-fleet")

	path, changed, err := WriteShim(ccf)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v", changed, err)
	}
	defsAssertShim(t, path, ccf, 0o755)

	if _, changed, err := WriteShim(ccf); err != nil || changed {
		t.Fatalf("identical rewrite: changed=%v err=%v", changed, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := WriteShim(ccf); err != nil || !changed {
		t.Fatalf("mode repair: changed=%v err=%v", changed, err)
	}
	defsAssertShim(t, path, ccf, 0o755)

	other := filepath.Join(h.Home, "new", "cc-fleet")
	if _, changed, err := WriteShim(other); err != nil || !changed {
		t.Fatalf("repin: changed=%v err=%v", changed, err)
	}
	defsAssertShim(t, path, other, 0o755)
}

// defsAssertShim checks the shim at path is RenderShim(ccf) at mode.
func defsAssertShim(t *testing.T, path, ccf string, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != mode {
		t.Fatalf("shim mode = %v, want %v", fi.Mode().Perm(), mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, RenderShim(ccf)) {
		t.Fatalf("shim content is not RenderShim(%q)", ccf)
	}
}
