package leadsession

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDetectFromPIDFindsAncestorSession(t *testing.T) {
	requireLinux(t)

	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	writeProc(t, root, 300, 200, "30300", "/bin/sh")
	writeProc(t, root, 200, 100, "20200", "/root/.local/share/claude/versions/2.1.152")
	writeProc(t, root, 100, 1, "10100", "/sbin/init")
	writeSession(t, filepath.Join(home, ".claude"), 200, "session-200", "20200")

	if got := DetectFromPID(300); got != "session-200" {
		t.Fatalf("DetectFromPID = %q, want session-200", got)
	}
}

func TestDetectFromPIDRejectsRecycledPID(t *testing.T) {
	requireLinux(t)

	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	writeProc(t, root, 300, 200, "30300", "/bin/sh")
	writeProc(t, root, 200, 1, "different-start", "/root/.local/share/claude/versions/2.1.152")
	writeSession(t, filepath.Join(home, ".claude"), 200, "stale-session", "original-start")

	if got := DetectFromPID(300); got != "" {
		t.Fatalf("DetectFromPID should reject stale session file, got %q", got)
	}
}

func TestDetectFromPIDSupportsClaudeConfigDir(t *testing.T) {
	requireLinux(t)

	cfg := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	writeProc(t, root, 42, 1, "4242", "claude")
	writeSession(t, cfg, 42, "cfg-session", "4242")

	if got := DetectFromPID(42); got != "cfg-session" {
		t.Fatalf("DetectFromPID = %q, want cfg-session", got)
	}
}

func TestDetectFromPIDRejectsSessionWithoutProcStart(t *testing.T) {
	requireLinux(t)

	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	writeProc(t, root, 42, 1, "4242", "claude")
	writeSession(t, filepath.Join(home, ".claude"), 42, "missing-proc-start", "")
	if got := DetectFromPID(42); got != "" {
		t.Fatalf("DetectFromPID should reject session file without procStart, got %q", got)
	}
}

// TestDetectPID_FromValidatedAncestor mirrors DetectFromPID's happy path but
// asserts the PID surface — used by spawn permission inheritance to read the
// lead's cmdline. The fail-closed branches (no procStart, recycled PID, missing
// session file) must all yield 0, not a stale PID.
func TestDetectPID_FromValidatedAncestor(t *testing.T) {
	requireLinux(t)

	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	// Walk should find the validated ancestor (pid 200) and return that pid.
	writeProc(t, root, 300, 200, "30300", "/bin/sh")
	writeProc(t, root, 200, 100, "20200", "/root/.local/share/claude/versions/2.1.152")
	writeProc(t, root, 100, 1, "10100", "/sbin/init")
	writeSession(t, filepath.Join(home, ".claude"), 200, "session-200", "20200")

	if got := walkPIDFrom(300); got != 200 {
		t.Fatalf("walk pid from 300 = %d, want 200", got)
	}

	// Recycled PID: session file's procStart no longer matches /proc — must
	// return 0, never the stale ancestor pid.
	root2 := t.TempDir()
	procRoot = root2
	writeProc(t, root2, 300, 200, "30300", "/bin/sh")
	writeProc(t, root2, 200, 1, "different-start", "/root/.local/share/claude/versions/2.1.152")
	writeSession(t, filepath.Join(home, ".claude"), 200, "stale-session", "original-start")
	if got := walkPIDFrom(300); got != 0 {
		t.Fatalf("walk pid with recycled session = %d, want 0", got)
	}

	// No session file at all on the chain → return 0.
	root3 := t.TempDir()
	procRoot = root3
	writeProc(t, root3, 300, 100, "30300", "/bin/sh")
	writeProc(t, root3, 100, 1, "10100", "/sbin/init")
	if got := walkPIDFrom(300); got != 0 {
		t.Fatalf("walk pid with no session = %d, want 0", got)
	}
}

// TestDetectPIDWithStart_AndRevalidate: DetectPIDWithStart returns the validated
// lead PID + its /proc start time, and RevalidateProcStart reports true only
// while the PID still names the same process and false once it's recycled.
func TestDetectPIDWithStart_AndRevalidate(t *testing.T) {
	requireLinux(t)

	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	origProcRoot := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = origProcRoot })

	// pid 200 is the validated Claude ancestor with start time "20200".
	writeProc(t, root, 200, 1, "20200", "/root/.local/share/claude/versions/2.1.152")
	writeSession(t, filepath.Join(home, ".claude"), 200, "session-200", "20200")

	pid, start := detectPIDWithStartFrom(200)
	if pid != 200 || start != "20200" {
		t.Fatalf("detectPIDWithStartFrom = (%d, %q), want (200, 20200)", pid, start)
	}

	// Same start time → still the same process → revalidate true.
	if !RevalidateProcStart(pid, start) {
		t.Fatalf("RevalidateProcStart(matching start) = false, want true")
	}

	// Simulate PID reuse: pid 200 now has a DIFFERENT start time → revalidate
	// must fail (the recycled-PID case inherit relies on).
	root2 := t.TempDir()
	procRoot = root2
	writeProc(t, root2, 200, 1, "99999", "/usr/bin/unrelated")
	if RevalidateProcStart(pid, start) {
		t.Fatalf("RevalidateProcStart(changed start) = true, want false (PID reuse must not validate)")
	}

	// Empty / non-positive inputs are always false.
	if RevalidateProcStart(0, "x") || RevalidateProcStart(200, "") {
		t.Fatalf("RevalidateProcStart with empty inputs must be false")
	}
}

// detectPIDWithStartFrom is a test seam onto DetectPIDWithStart's walk so we can
// assert without depending on os.Getppid().
func detectPIDWithStartFrom(start int) (int, string) {
	id, p := walk(start)
	if id == "" || p == 0 {
		return 0, ""
	}
	st, ok := procStart(p)
	if !ok {
		return 0, ""
	}
	return p, st
}

// TestPIDWalk_UnsupportedPlatform_ReturnsZero locks the auto-degrade guarantee
// for platforms with NO process introspection (procintrospect's "other" build):
// parentPID/procStart return (_,false) so walk bails on the first step. linux,
// darwin, and windows all have process introspection, so they're excluded here
// and covered by their own tests.
func TestPIDWalk_UnsupportedPlatform_ReturnsZero(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("introspection-capable platform; degrade is asserted on 'other' only")
	}
	// On an unsupported host parentPID returns (0,false) for any pid; walk falls
	// out before reaching any session file lookup.
	if got := walkPIDFrom(os.Getpid()); got != 0 {
		t.Fatalf("PID walk on an unsupported platform = %d, want 0", got)
	}
}

// walkPIDFrom is the test seam onto walk() so we can assert the PID surface
// without depending on os.Getppid(). Keeps the production API surface clean.
func walkPIDFrom(pid int) int {
	_, p := walk(pid)
	return p
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("lead session auto-detection currently uses Linux /proc")
	}
}

func writeProc(t *testing.T, root string, pid, ppid int, start string, argv0 string) {
	t.Helper()
	dir := filepath.Join(root, itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields 1..22 are enough for parentPID and procStart. The second field is
	// comm; include a space to cover Linux's special stat parsing shape.
	stat := itoa(pid) + " (proc name) S " + itoa(ppid)
	for i := 5; i <= 21; i++ {
		stat += " 0"
	}
	stat += " " + start + " 0\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(argv0+"\x00--flag"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSession(t *testing.T, cfg string, pid int, sessionID, procStart string) {
	t.Helper()
	dir := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"pid":` + itoa(pid) + `,"sessionId":"` + sessionID + `"`
	if procStart != "" {
		body += `,"procStart":"` + procStart + `"`
	}
	body += `}`
	if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
