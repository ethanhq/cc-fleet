package leadsession

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

func TestDetectFromPIDFindsAncestorSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	procs := stubProcs(t)
	procs.add(300, 200, "30300")
	procs.add(200, 100, "20200")
	procs.add(100, 1, "10100")
	writeSession(t, filepath.Join(home, ".claude"), 200, "session-200", "20200")

	if got := DetectFromPID(300); got != "session-200" {
		t.Fatalf("DetectFromPID = %q, want session-200", got)
	}
}

func TestCodexThread(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "")
	if got := CodexThread(); got != "" {
		t.Fatalf("CodexThread empty env = %q, want \"\"", got)
	}
	t.Setenv("CODEX_THREAD_ID", "  019f094d-thread  ")
	if got := CodexThread(); got != "codex:019f094d-thread" {
		t.Fatalf("CodexThread = %q, want codex:019f094d-thread", got)
	}
	t.Setenv("CODEX_THREAD_ID", "ab\x07cd")
	if got := CodexThread(); got != "codex:abcd" {
		t.Fatalf("CodexThread control-byte strip = %q, want codex:abcd", got)
	}
}

func TestDetectFromPIDRejectsRecycledPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	procs := stubProcs(t)
	procs.add(300, 200, "30300")
	procs.add(200, 1, "different-start")
	writeSession(t, filepath.Join(home, ".claude"), 200, "stale-session", "original-start")

	if got := DetectFromPID(300); got != "" {
		t.Fatalf("DetectFromPID should reject stale session file, got %q", got)
	}
}

func TestDetectFromPIDSupportsClaudeConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	procs := stubProcs(t)
	procs.add(42, 1, "4242")
	writeSession(t, cfg, 42, "cfg-session", "4242")

	if got := DetectFromPID(42); got != "cfg-session" {
		t.Fatalf("DetectFromPID = %q, want cfg-session", got)
	}
}

func TestDetectFromPIDRejectsSessionWithoutProcStart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	procs := stubProcs(t)
	procs.add(42, 1, "4242")
	writeSession(t, filepath.Join(home, ".claude"), 42, "missing-proc-start", "")
	if got := DetectFromPID(42); got != "" {
		t.Fatalf("DetectFromPID should reject session file without procStart, got %q", got)
	}
}

// TestDetectPID_FromValidatedAncestor mirrors DetectFromPID's happy path but
// asserts the PID surface. The fail-closed branches (no procStart, recycled PID, missing
// session file) must all yield 0, not a stale PID.
func TestDetectPID_FromValidatedAncestor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows reads USERPROFILE
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	// Walk should find the validated ancestor (pid 200) and return that pid.
	procs := stubProcs(t)
	procs.add(300, 200, "30300")
	procs.add(200, 100, "20200")
	procs.add(100, 1, "10100")
	writeSession(t, filepath.Join(home, ".claude"), 200, "session-200", "20200")

	if got := walkPIDFrom(300); got != 200 {
		t.Fatalf("walk pid from 300 = %d, want 200", got)
	}

	// Recycled PID: session file's procStart no longer matches the process —
	// must return 0, never the stale ancestor pid.
	procs2 := stubProcs(t)
	procs2.add(300, 200, "30300")
	procs2.add(200, 1, "different-start")
	writeSession(t, filepath.Join(home, ".claude"), 200, "stale-session", "original-start")
	if got := walkPIDFrom(300); got != 0 {
		t.Fatalf("walk pid with recycled session = %d, want 0", got)
	}

	// No session file at all on the chain → return 0.
	procs3 := stubProcs(t)
	procs3.add(300, 100, "30300")
	procs3.add(100, 1, "10100")
	if got := walkPIDFrom(300); got != 0 {
		t.Fatalf("walk pid with no session = %d, want 0", got)
	}
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
	s, _ := walk(pid)
	return s.PID
}

// TestSessionFileValidatesProcStart: SessionFile returns the full registry
// entry only when the file exists, the pid is alive and procStart matches.
func TestSessionFileValidatesProcStart(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	want := Session{
		PID: 42, SessionID: "sess-42", Cwd: "/work/a", StartedAt: 1_700_000_000_000,
		ProcStart: "4242", Version: "2.1.281", Kind: "interactive", Entrypoint: "cli",
	}
	writeSessionJSON(t, cfg, want)
	procs := stubProcs(t)
	procs.add(42, 1, "4242")

	if got, ok := SessionFile(42); !ok || got != want {
		t.Fatalf("SessionFile(42) = %+v, %v; want %+v, true", got, ok, want)
	}

	procs.add(42, 1, "9999") // pid reused by another process
	if got, ok := SessionFile(42); ok {
		t.Fatalf("SessionFile with mismatched procStart = %+v, want !ok", got)
	}

	delete(procs, 42) // process gone
	if got, ok := SessionFile(42); ok {
		t.Fatalf("SessionFile for dead pid = %+v, want !ok", got)
	}

	procs.add(43, 1, "4343")
	if got, ok := SessionFile(43); ok {
		t.Fatalf("SessionFile without a file = %+v, want !ok", got)
	}

	noStart := want
	noStart.PID, noStart.SessionID, noStart.ProcStart = 44, "sess-44", ""
	writeSessionJSON(t, cfg, noStart)
	procs.add(44, 1, "4444")
	if got, ok := SessionFile(44); ok {
		t.Fatalf("SessionFile without procStart = %+v, want !ok", got)
	}

	other := want
	other.PID, other.SessionID, other.ProcStart = 46, "sess-45", "4545"
	writeSessionJSONAs(t, cfg, 45, other) // file for 45 claims pid 46
	procs.add(45, 1, "4545")
	if got, ok := SessionFile(45); ok {
		t.Fatalf("SessionFile with foreign pid field = %+v, want !ok", got)
	}
}

// TestLiveSessionsDropsReusedPID: a registry file whose pid now belongs to an
// unrelated process (design F33) is dropped; non-registry files are ignored.
func TestLiveSessionsDropsReusedPID(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	live := Session{PID: 100, SessionID: "live", Cwd: "/w", StartedAt: 1, ProcStart: "p100"}
	stale := Session{PID: 32645, SessionID: "stale", Cwd: "/w", StartedAt: 1, ProcStart: "old-start"}
	writeSessionJSON(t, cfg, live)
	writeSessionJSON(t, cfg, stale)
	dir := filepath.Join(cfg, "sessions")
	for name, body := range map[string]string{"notes.txt": "x", "abc.json": "{}", "7.json": "not json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	procs := stubProcs(t)
	procs.add(100, 1, "p100")
	procs.add(32645, 1, "promotedcontentd-start")
	procs.add(7, 1, "p7")

	got := LiveSessions()
	if len(got) != 1 || got[0] != live {
		t.Fatalf("LiveSessions = %+v, want only %+v", got, live)
	}
}

func TestBySessionID(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	a := Session{PID: 100, SessionID: "aaaa-1", ProcStart: "p100"}
	b := Session{PID: 200, SessionID: "bbbb-2", ProcStart: "p200"}
	stale := Session{PID: 300, SessionID: "cccc-3", ProcStart: "p300"}
	for _, s := range []Session{a, b, stale} {
		writeSessionJSON(t, cfg, s)
	}
	procs := stubProcs(t)
	procs.add(100, 1, "p100")
	procs.add(200, 1, "p200")
	procs.add(300, 1, "reused")

	if got, ok := BySessionID("bbbb-2"); !ok || got != b {
		t.Fatalf("BySessionID(bbbb-2) = %+v, %v; want %+v", got, ok, b)
	}
	for _, id := range []string{"", "bbbb", "cccc-3", "missing"} {
		if got, ok := BySessionID(id); ok {
			t.Errorf("BySessionID(%q) = %+v, want !ok", id, got)
		}
	}
}

// TestRegisteredSessions: every registry file counts, live or not; other
// files and unparseable ones are skipped.
func TestRegisteredSessions(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if got := RegisteredSessions(); len(got) != 0 {
		t.Fatalf("RegisteredSessions without a registry = %v, want empty", got)
	}

	a := Session{PID: 100, SessionID: "aaaa-1", Cwd: "/work/a", StartedAt: 1_700_000_000_000, ProcStart: "p100", Entrypoint: "cli"}
	dead := Session{PID: 300, SessionID: "cccc-3"} // no procStart, process gone
	writeSessionJSON(t, cfg, a)
	writeSessionJSON(t, cfg, dead)
	for name, body := range map[string]string{"400.json": "{not json", "notes.txt": `{"sessionId":"x"}`, "abc.json": `{"sessionId":"y"}`} {
		if err := os.WriteFile(filepath.Join(cfg, "sessions", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := RegisteredSessions()
	sort.Slice(got, func(i, j int) bool { return got[i].PID < got[j].PID })
	if len(got) != 2 || got[0] != a || got[1] != dead {
		t.Fatalf("RegisteredSessions = %+v, want %+v and %+v", got, a, dead)
	}
}

// TestDetectSessionWalksAncestors: DetectSession starts at the real parent pid
// and returns the first ancestor's full registry entry.
func TestDetectSessionWalksAncestors(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	parent := os.Getppid()
	lead := Session{
		PID: 1_000_001, SessionID: "lead-session", Cwd: "/work/p", StartedAt: 1_700_000_000_000,
		ProcStart: "lead-start", Version: "2.1.281", Kind: "interactive", Entrypoint: "cli",
	}
	writeSessionJSON(t, cfg, lead)
	procs := stubProcs(t)
	procs.add(parent, 1_000_000, "parent-start")
	procs.add(1_000_000, lead.PID, "shell-start")
	procs.add(lead.PID, 1, "lead-start")

	if got, ok := DetectSession(); !ok || got != lead {
		t.Fatalf("DetectSession = %+v, %v; want %+v", got, ok, lead)
	}

	procs.add(lead.PID, 1, "reused")
	if got, ok := DetectSession(); ok {
		t.Fatalf("DetectSession with reused lead pid = %+v, want !ok", got)
	}
}

// fakeProcs is a fake process table read by the ppidFn/procStartFn stubs.
type fakeProcs map[int]fakeProc

type fakeProc struct {
	ppid  int
	start string
}

func (p fakeProcs) add(pid, ppid int, start string) { p[pid] = fakeProc{ppid, start} }

// stubProcs points ppidFn/procStartFn at a new, empty fake process table.
func stubProcs(t *testing.T) fakeProcs {
	t.Helper()
	procs := fakeProcs{}
	origPpid, origStart := ppidFn, procStartFn
	t.Cleanup(func() { ppidFn, procStartFn = origPpid, origStart })
	ppidFn = func(pid int) (int, bool) {
		p, ok := procs[pid]
		return p.ppid, ok
	}
	procStartFn = func(pid int) (string, bool) {
		p, ok := procs[pid]
		return p.start, ok
	}
	return procs
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

// writeSessionJSON writes s as sessions/<s.PID>.json.
func writeSessionJSON(t *testing.T, cfg string, s Session) {
	t.Helper()
	writeSessionJSONAs(t, cfg, s.PID, s)
}

// writeSessionJSONAs writes s as sessions/<pid>.json.
func writeSessionJSONAs(t *testing.T, cfg string, pid int, s Session) {
	t.Helper()
	dir := filepath.Join(cfg, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, itoa(pid)+".json"), data, 0o600); err != nil {
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
