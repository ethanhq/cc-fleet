package teardown

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/spawn"
)

// sigCall records one (pid, signal) the code under test sent.
type sigCall struct {
	pid int
	sig syscall.Signal
}

// reapHarness swaps the process-reaping seams so tests never signal a real
// process. Scripted via its maps; recorded signals land in calls.
type reapHarness struct {
	pids    map[string][]int // agentID -> discovered pids
	survive map[int]bool     // pids still alive after SIGTERM (force SIGKILL)
	termErr map[int]error    // pids whose SIGTERM returns this error
	calls   []sigCall
}

// installReapHarness replaces findTeammatePIDs / signalProc / procReapGrace
// for the test and restores them on cleanup. Called with no scripting it
// simply disarms reaping (finds nothing), which is what the pre-existing
// teardown tests want.
func installReapHarness(t *testing.T) *reapHarness {
	t.Helper()
	h := &reapHarness{
		pids:    map[string][]int{},
		survive: map[int]bool{},
		termErr: map[int]error{},
	}
	origFind, origSig, origGrace := findTeammatePIDs, signalProc, procReapGrace
	t.Cleanup(func() {
		findTeammatePIDs, signalProc, procReapGrace = origFind, origSig, origGrace
	})
	procReapGrace = time.Millisecond // keep the SIGTERM→SIGKILL wait tiny
	findTeammatePIDs = func(agentID string) []int { return h.pids[agentID] }
	signalProc = func(pid int, sig syscall.Signal) error {
		h.calls = append(h.calls, sigCall{pid, sig})
		switch sig {
		case syscall.SIGTERM:
			if e, ok := h.termErr[pid]; ok {
				return e
			}
			return nil
		case 0: // liveness probe
			if h.survive[pid] {
				return nil // still alive
			}
			return syscall.ESRCH // exited
		default: // SIGKILL etc.
			return nil
		}
	}
	return h
}

// sigs returns the signals sent to pid, in order.
func (h *reapHarness) sigs(pid int) []syscall.Signal {
	var out []syscall.Signal
	for _, c := range h.calls {
		if c.pid == pid {
			out = append(out, c.sig)
		}
	}
	return out
}

// installFakeTmux writes a small POSIX shell script named `tmux` into a
// fresh dir and prepends that dir to $PATH so internal/tmux (and the
// teardown package) calls it instead of the real binary. The script
// records its argv to MOCK_ARGS_FILE and uses MOCK_EXIT_CODE +
// MOCK_OUTPUT_FILE to script behavior — same scheme as
// internal/tmux/tmux_test.go::mockTmux but inlined here to avoid coupling
// the two test files.
func installFakeTmux(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.log")
	binPath := filepath.Join(dir, "tmux")
	script := `#!/bin/sh
for a in "$@"; do
  printf '%s\n' "$a" >> "$MOCK_ARGS_FILE"
done
printf '__END__\n' >> "$MOCK_ARGS_FILE"
if [ -n "$MOCK_OUTPUT_FILE" ] && [ -f "$MOCK_OUTPUT_FILE" ]; then
  cat "$MOCK_OUTPUT_FILE"
fi
exit "${MOCK_EXIT_CODE:-0}"
`
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MOCK_ARGS_FILE", argsPath)
	t.Setenv("MOCK_OUTPUT_FILE", "")
	t.Setenv("MOCK_EXIT_CODE", "0")
	return argsPath
}

// seedTeam writes a team config with the given members at the standard
// $HOME/.claude/teams/<team>/config.json location, plus an inbox file
// per named member. Returns the absolute path to the team directory.
func seedTeam(t *testing.T, team string, members []spawn.Member) string {
	t.Helper()
	tc := &spawn.TeamConfig{
		LeadSessionID: "lead-uuid",
		Members:       members,
		Raw:           map[string]any{},
	}
	if err := spawn.EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	if err := spawn.WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	for _, m := range members {
		if m.Name == "" {
			continue
		}
		if err := spawn.EnsureInbox(team, m.Name); err != nil {
			t.Fatalf("EnsureInbox %s: %v", m.Name, err)
		}
	}
	dir, _ := spawn.TeamDir(team)
	return dir
}

// seedSwarmTeam writes a team config whose Raw carries a swarm socket name
// (out-of-tmux), so teardown sees the team as swarm-backed.
func seedSwarmTeam(t *testing.T, team, socket string, members []spawn.Member) {
	t.Helper()
	tc := &spawn.TeamConfig{LeadSessionID: "lead-uuid", Members: members, Raw: map[string]any{}}
	tc.SetTmuxSocket(socket)
	if err := spawn.EnsureTeamDir(team); err != nil {
		t.Fatalf("EnsureTeamDir: %v", err)
	}
	if err := spawn.WriteTeamConfig(team, tc); err != nil {
		t.Fatalf("WriteTeamConfig: %v", err)
	}
	for _, m := range members {
		if m.Name != "" {
			if err := spawn.EnsureInbox(team, m.Name); err != nil {
				t.Fatalf("EnsureInbox %s: %v", m.Name, err)
			}
		}
	}
}

// readFakeTmuxCalls parses installFakeTmux's args.log into per-invocation argv
// (same __END__-delimited format the fake tmux script writes).
func readFakeTmuxCalls(t *testing.T, path string) [][]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read args.log: %v", err)
	}
	var calls [][]string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "__END__" {
			calls = append(calls, cur)
			cur = nil
			continue
		}
		if line == "" {
			continue
		}
		cur = append(cur, line)
	}
	return calls
}

// containsCall reports whether any recorded invocation's argv exactly equals want.
func containsCall(calls [][]string, want []string) bool {
	for _, c := range calls {
		if reflect.DeepEqual(c, want) {
			return true
		}
	}
	return false
}

// hasToken reports whether tok appears anywhere in any recorded invocation.
func hasToken(calls [][]string, tok string) bool {
	for _, c := range calls {
		for _, a := range c {
			if a == tok {
				return true
			}
		}
	}
	return false
}

func TestTeardownTeam_RemovesEverything(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	installReapHarness(t) // disarm reaping; this test doesn't exercise it

	dir := seedTeam(t, "alpha", []spawn.Member{
		{Name: "a1", AgentID: "a1@alpha", TmuxPaneID: "%10", AgentType: "general-purpose"},
		{Name: "a2", AgentID: "a2@alpha", TmuxPaneID: "%11", AgentType: "general-purpose"},
	})

	res := TeardownTeam("alpha")
	if !res.OK {
		t.Fatalf("TeardownTeam: ok=false code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if !res.TeamRemoved {
		t.Fatal("TeamRemoved should be true")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("team dir still exists: %v", err)
	}
	// Both panes / both members must be reported as torn down, by identity (not
	// just count): a bug that double-reported one pane and dropped the other
	// would still yield len 2. Order follows config member order (a1, a2).
	if want := []string{"%10", "%11"}; !reflect.DeepEqual(res.Panes, want) {
		t.Fatalf("Panes = %v, want %v", res.Panes, want)
	}
	if want := []string{"a1", "a2"}; !reflect.DeepEqual(res.Members, want) {
		t.Fatalf("Members = %v, want %v", res.Members, want)
	}
}

// TestTeardownTeam_SwarmSocketKillAndKillServer: an out-of-tmux swarm team kills
// each pane on its PRIVATE socket (-L cc-fleet-swarm-<team>) and, once members
// are gone, kills that server — the no-leak path at unit level. Without
// socket-scoping, a default-server kill-pane on a swarm pane would "can't find"
// → swallowed → silent leak.
func TestTeardownTeam_SwarmSocketKillAndKillServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	const sock = "cc-fleet-swarm-swarmt"
	seedSwarmTeam(t, "swarmt", sock, []spawn.Member{
		{Name: "w1", AgentID: "w1@swarmt", TmuxPaneID: "%0", AgentType: "general-purpose"},
		{Name: "w2", AgentID: "w2@swarmt", TmuxPaneID: "%1", AgentType: "general-purpose"},
	})

	res := TeardownTeam("swarmt")
	if !res.OK {
		t.Fatalf("teardown ok=false: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	calls := readFakeTmuxCalls(t, argsPath)
	for _, pane := range []string{"%0", "%1"} {
		want := []string{"-L", sock, "kill-pane", "-t", pane}
		if !containsCall(calls, want) {
			t.Fatalf("missing socket-scoped kill-pane for %s; calls=%v", pane, calls)
		}
	}
	if !containsCall(calls, []string{"-L", sock, "kill-server"}) {
		t.Fatalf("missing socket kill-server after members cleared; calls=%v", calls)
	}
}

// TestTeardownTeam_InTmuxNoKillServer: an in-tmux team kills panes on the DEFAULT
// server (no -L) and must NEVER kill-server (that would kill the user's own tmux).
func TestTeardownTeam_InTmuxNoKillServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	argsPath := installFakeTmux(t)
	installReapHarness(t)

	seedTeam(t, "alpha", []spawn.Member{
		{Name: "a1", AgentID: "a1@alpha", TmuxPaneID: "%10", AgentType: "general-purpose"},
	})
	res := TeardownTeam("alpha")
	if !res.OK {
		t.Fatalf("teardown ok=false: %s", res.ErrorMsg)
	}
	calls := readFakeTmuxCalls(t, argsPath)
	if !containsCall(calls, []string{"kill-pane", "-t", "%10"}) {
		t.Fatalf("missing default-server kill-pane (no -L); calls=%v", calls)
	}
	if hasToken(calls, "kill-server") {
		t.Fatalf("in-tmux teardown must NEVER kill-server; calls=%v", calls)
	}
}

func TestTeardownTeam_MissingIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)

	res := TeardownTeam("never-existed")
	if !res.OK {
		t.Fatalf("idempotent teardown should return OK: code=%s msg=%s",
			res.ErrorCode, res.ErrorMsg)
	}
	if res.TeamRemoved {
		t.Fatal("TeamRemoved should be false when nothing was there")
	}
	if len(res.Panes) != 0 || len(res.Members) != 0 {
		t.Fatalf("Panes=%v Members=%v, want empty", res.Panes, res.Members)
	}
}

func TestTeardownTeam_EmptyName(t *testing.T) {
	res := TeardownTeam("")
	if res.OK {
		t.Fatal("empty team name should fail")
	}
	if res.ErrorCode != ErrCodeInternal {
		t.Fatalf("ErrorCode = %q, want %q", res.ErrorCode, ErrCodeInternal)
	}
}

func TestTeardownTeam_TmuxKillFailureIsWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	installReapHarness(t) // disarm reaping; this test only checks the tmux warning
	// All tmux invocations report "can't find pane" — KillPane treats that
	// as success, so we instead make tmux fail with a real-looking error
	// to exercise the warning path.
	t.Setenv("MOCK_EXIT_CODE", "1")
	scratch := t.TempDir()
	errOut := filepath.Join(scratch, "stderr.txt")
	_ = os.WriteFile(errOut, []byte("server not running\n"), 0o644)
	t.Setenv("MOCK_OUTPUT_FILE", errOut)

	seedTeam(t, "beta", []spawn.Member{
		{Name: "b1", AgentID: "b1@beta", TmuxPaneID: "%20", AgentType: "general-purpose"},
	})

	res := TeardownTeam("beta")
	if !res.OK {
		t.Fatalf("tmux kill warning should not fail teardown: code=%s msg=%s",
			res.ErrorCode, res.ErrorMsg)
	}
	if !res.TeamRemoved {
		t.Fatal("team dir should still get removed when only tmux fails")
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected at least one warning about kill failure")
	}
}

func TestTeardownPane_HappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	installReapHarness(t) // disarm reaping; covered by TestTeardownPane_ReapsOnlyItsMember

	seedTeam(t, "gamma", []spawn.Member{
		{Name: "g1", AgentID: "g1@gamma", TmuxPaneID: "%30", AgentType: "general-purpose"},
		{Name: "g2", AgentID: "g2@gamma", TmuxPaneID: "%31", AgentType: "general-purpose"},
	})

	res := TeardownPane("%30")
	if !res.OK {
		t.Fatalf("TeardownPane: code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if len(res.Members) != 1 || res.Members[0] != "g1" {
		t.Fatalf("Members = %v, want [g1]", res.Members)
	}

	// Team config should still have g2, lost g1.
	tc, err := spawn.LoadTeamConfig("gamma")
	if err != nil {
		t.Fatalf("LoadTeamConfig: %v", err)
	}
	if len(tc.Members) != 1 || tc.Members[0].Name != "g2" {
		t.Fatalf("members after teardown = %+v, want only g2", tc.Members)
	}

	// g1's inbox should be gone.
	inboxPath, _ := spawn.InboxPath("gamma", "g1")
	if _, err := os.Stat(inboxPath); !os.IsNotExist(err) {
		t.Fatalf("g1 inbox still exists: %v", err)
	}
	// g2's inbox should still exist.
	g2Inbox, _ := spawn.InboxPath("gamma", "g2")
	if _, err := os.Stat(g2Inbox); err != nil {
		t.Fatalf("g2 inbox missing: %v", err)
	}
}

func TestTeardownPane_PaneNotFoundIsIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	// tmux returns "can't find pane" → KillPane treats as success.
	t.Setenv("MOCK_EXIT_CODE", "1")
	scratch := t.TempDir()
	errOut := filepath.Join(scratch, "stderr.txt")
	_ = os.WriteFile(errOut, []byte("can't find pane: %99\n"), 0o644)
	t.Setenv("MOCK_OUTPUT_FILE", errOut)

	res := TeardownPane("%99")
	if !res.OK {
		t.Fatalf("nonexistent pane should be OK (idempotent): code=%s msg=%s",
			res.ErrorCode, res.ErrorMsg)
	}
}

func TestTeardownPane_NoOwningTeamStillKillsPane(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	// No teams seeded; just call TeardownPane.

	res := TeardownPane("%50")
	if !res.OK {
		t.Fatalf("orphan pane teardown should be OK: code=%s msg=%s",
			res.ErrorCode, res.ErrorMsg)
	}
	if len(res.Members) != 0 {
		t.Fatalf("Members = %v, want empty (no team claimed pane)", res.Members)
	}
}

func TestTeardownPane_EmptyPaneID(t *testing.T) {
	res := TeardownPane("")
	if res.OK {
		t.Fatal("empty pane id should fail")
	}
}

func TestTeardownPane_RejectsBareName(t *testing.T) {
	// Bare names (no leading %) must be rejected — they would otherwise
	// silently look up no team and "succeed" misleadingly.
	res := TeardownPane("alpha")
	if res.OK {
		t.Fatal("non-% pane id should fail")
	}
	if !strings.Contains(res.ErrorMsg, "%") {
		t.Fatalf("error should mention %% requirement: %q", res.ErrorMsg)
	}
}

func TestFindPaneOwner_NoTeamsRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Don't seed anything — teams root doesn't exist.
	team, name, socket, err := findPaneOwner("%1")
	if err != nil {
		t.Fatalf("findPaneOwner: %v", err)
	}
	if team != "" || name != "" || socket != "" {
		t.Fatalf("got (%q,%q,%q), want empty", team, name, socket)
	}
}

func TestFindPaneOwner_Match(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTeam(t, "delta", []spawn.Member{
		{Name: "d1", AgentID: "d1@delta", TmuxPaneID: "%77", AgentType: "general-purpose"},
	})
	team, name, socket, err := findPaneOwner("%77")
	if err != nil {
		t.Fatalf("findPaneOwner: %v", err)
	}
	if team != "delta" || name != "d1" {
		t.Fatalf("got (%q,%q), want (delta,d1)", team, name)
	}
	if socket != "" {
		t.Fatalf("in-tmux team should have empty socket, got %q", socket)
	}
}

func TestFindPaneOwner_NoMatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedTeam(t, "delta", []spawn.Member{
		{Name: "d1", AgentID: "d1@delta", TmuxPaneID: "%77", AgentType: "general-purpose"},
	})
	team, name, socket, err := findPaneOwner("%999")
	if err != nil {
		t.Fatalf("findPaneOwner: %v", err)
	}
	if team != "" || name != "" || socket != "" {
		t.Fatalf("got (%q,%q,%q), want empty for unknown pane", team, name, socket)
	}
}

// ----- process reaping -----

func TestReapTeammateProcess_GracefulSIGTERM(t *testing.T) {
	h := installReapHarness(t)
	h.pids["a@t"] = []int{4242} // not in survive → exits on SIGTERM

	killed, warns := reapTeammateProcess("a@t")
	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	if !reflect.DeepEqual(killed, []int{4242}) {
		t.Fatalf("killed = %v, want [4242]", killed)
	}
	for _, s := range h.sigs(4242) {
		if s == syscall.SIGKILL {
			t.Fatalf("unexpected SIGKILL for a process that exited on SIGTERM; sigs=%v", h.sigs(4242))
		}
	}
	if got := h.sigs(4242); len(got) == 0 || got[0] != syscall.SIGTERM {
		t.Fatalf("sigs = %v, want to start with SIGTERM", got)
	}
}

func TestReapTeammateProcess_EscalatesToSIGKILL(t *testing.T) {
	h := installReapHarness(t)
	h.pids["a@t"] = []int{4242}
	h.survive[4242] = true // still alive after SIGTERM → must be SIGKILLed

	killed, warns := reapTeammateProcess("a@t")
	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none", warns)
	}
	if !reflect.DeepEqual(killed, []int{4242}) {
		t.Fatalf("killed = %v, want [4242]", killed)
	}
	sawKill := false
	for _, s := range h.sigs(4242) {
		if s == syscall.SIGKILL {
			sawKill = true
		}
	}
	if !sawKill {
		t.Fatalf("expected SIGKILL escalation; sigs=%v", h.sigs(4242))
	}
}

func TestReapTeammateProcess_NoProcessFound(t *testing.T) {
	h := installReapHarness(t)
	// No pids scripted for this id → nothing to do, no panic, no signals.
	killed, warns := reapTeammateProcess("ghost@none")
	if len(killed) != 0 || len(warns) != 0 {
		t.Fatalf("killed=%v warns=%v, want both empty", killed, warns)
	}
	if len(h.calls) != 0 {
		t.Fatalf("no signals expected; got %v", h.calls)
	}
}

func TestReapTeammateProcess_SIGTERMErrorIsWarning(t *testing.T) {
	h := installReapHarness(t)
	h.pids["a@t"] = []int{4242}
	h.termErr[4242] = syscall.EPERM // can't signal → best-effort warning

	killed, warns := reapTeammateProcess("a@t")
	if len(killed) != 0 {
		t.Fatalf("killed = %v, want empty (SIGTERM failed)", killed)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warns)
	}
}

func TestReapTeammateProcess_AlreadyGoneIsNotWarned(t *testing.T) {
	h := installReapHarness(t)
	h.pids["a@t"] = []int{4242}
	h.termErr[4242] = syscall.ESRCH // raced us and already exited

	killed, warns := reapTeammateProcess("a@t")
	if len(killed) != 0 {
		t.Fatalf("killed = %v, want empty", killed)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none (ESRCH is benign)", warns)
	}
}

func TestTeardownTeam_ReapsMemberProcesses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	h := installReapHarness(t)
	h.pids["a1@alpha"] = []int{111}
	h.pids["a2@alpha"] = []int{222}

	seedTeam(t, "alpha", []spawn.Member{
		{Name: "a1", AgentID: "a1@alpha", TmuxPaneID: "%10", AgentType: "general-purpose"},
		{Name: "a2", AgentID: "a2@alpha", TmuxPaneID: "%11", AgentType: "general-purpose"},
	})

	res := TeardownTeam("alpha")
	if !res.OK {
		t.Fatalf("ok=false code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	// Members iterate in order, so both pids are reaped in order.
	if !reflect.DeepEqual(res.KilledPIDs, []int{111, 222}) {
		t.Fatalf("KilledPIDs = %v, want [111 222]", res.KilledPIDs)
	}
	if got := h.sigs(111); len(got) == 0 || got[0] != syscall.SIGTERM {
		t.Fatalf("pid 111 sigs = %v, want starting with SIGTERM", got)
	}
}

func TestTeardownPane_ReapsOnlyItsMember(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installFakeTmux(t)
	h := installReapHarness(t)
	h.pids["g1@gamma"] = []int{333}
	h.pids["g2@gamma"] = []int{444} // belongs to a different pane — must survive

	seedTeam(t, "gamma", []spawn.Member{
		{Name: "g1", AgentID: "g1@gamma", TmuxPaneID: "%30", AgentType: "general-purpose"},
		{Name: "g2", AgentID: "g2@gamma", TmuxPaneID: "%31", AgentType: "general-purpose"},
	})

	res := TeardownPane("%30")
	if !res.OK {
		t.Fatalf("ok=false code=%s msg=%s", res.ErrorCode, res.ErrorMsg)
	}
	if !reflect.DeepEqual(res.KilledPIDs, []int{333}) {
		t.Fatalf("KilledPIDs = %v, want [333]", res.KilledPIDs)
	}
	if len(h.sigs(444)) != 0 {
		t.Fatalf("g2 (pid 444) must not be signalled; calls=%v", h.calls)
	}
}
