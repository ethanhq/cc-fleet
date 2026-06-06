package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolateHome points $HOME at a fresh temp dir so teamLockPath is sandboxed.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestWithTeamLock_RunsFn(t *testing.T) {
	isolateHome(t)
	called := false
	err := WithTeamLock("teamA", func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithTeamLock: %v", err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

func TestWithTeamLock_CreatesLockFile(t *testing.T) {
	home := isolateHome(t)
	if err := WithTeamLock("teamA", func() error { return nil }); err != nil {
		t.Fatalf("WithTeamLock: %v", err)
	}
	path := filepath.Join(home, ".claude", "teams", "teamA", ".cc-fleet-lock")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	// NTFS reports 0666; the 0600 contract is unix-only.
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("lock file mode = %o, want 0600", got)
	}
}

func TestWithTeamLock_PropagatesFnError(t *testing.T) {
	isolateHome(t)
	sentinel := errors.New("boom")
	err := WithTeamLock("teamA", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps sentinel", err)
	}
}

func TestWithTeamLock_RejectsEmptyTeam(t *testing.T) {
	isolateHome(t)
	if err := WithTeamLock("", func() error { return nil }); err == nil {
		t.Fatal("WithTeamLock(\"\"): want error, got nil")
	}
}

func TestWithTeamLock_RejectsNilFn(t *testing.T) {
	isolateHome(t)
	if err := WithTeamLock("teamA", nil); err == nil {
		t.Fatal("WithTeamLock(_, nil): want error, got nil")
	}
}

func TestWithTeamLock_RequiresHome(t *testing.T) {
	t.Setenv("HOME", "")
	if err := WithTeamLock("teamA", func() error { return nil }); err == nil {
		t.Fatal("WithTeamLock with empty HOME: want error, got nil")
	}
}

// TestWithTeamLock_SerializesGoroutines is the load-bearing serialization test.
// Two goroutines each hold the lock for 100ms; we require total wall time
// >= 200ms, proving the kernel actually serializes the second behind the first.
//
// We use 180ms as the threshold (10% slack) to absorb scheduler jitter but
// still catch a lock that does nothing.
func TestWithTeamLock_SerializesGoroutines(t *testing.T) {
	isolateHome(t)
	const team = "race"
	const hold = 100 * time.Millisecond

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithTeamLock(team, func() error {
				time.Sleep(hold)
				return nil
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("WithTeamLock: %v", err)
	}

	elapsed := time.Since(start)
	min := 2*hold - 20*time.Millisecond // 10% jitter slack
	if elapsed < min {
		t.Fatalf("two 100ms holders finished in %v, want >= %v (lock did not serialize)",
			elapsed, min)
	}
}

// TestWithTeamLock_DifferentTeamsConcurrent verifies the lock is per-team,
// not global — two distinct teams can run in parallel.
func TestWithTeamLock_DifferentTeamsConcurrent(t *testing.T) {
	isolateHome(t)
	const hold = 100 * time.Millisecond

	start := time.Now()
	var wg sync.WaitGroup
	for _, team := range []string{"teamA", "teamB"} {
		team := team
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = WithTeamLock(team, func() error {
				time.Sleep(hold)
				return nil
			})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	// Two parallel 100ms holders on different locks should finish in well
	// under 2*hold. We allow up to 1.5*hold for scheduler overhead.
	max := 3 * hold / 2
	if elapsed > max {
		t.Fatalf("two parallel 100ms holders on different teams took %v, want < %v (locks not per-team)",
			elapsed, max)
	}
}

// TestWithServerLock_SerializesGlobally verifies the global server lock
// serializes ALL callers regardless of team — the guarantee per-team locks lack
// for cross-team spawns into the same tmux window (the split race). Two
// concurrent holders must run back-to-back, not in parallel. Contrast with
// TestWithTeamLock_DifferentTeamsConcurrent, which runs in parallel.
func TestWithServerLock_SerializesGlobally(t *testing.T) {
	isolateHome(t)
	const hold = 100 * time.Millisecond

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithServerLock(func() error {
				time.Sleep(hold)
				return nil
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("WithServerLock: %v", err)
	}

	elapsed := time.Since(start)
	min := 2*hold - 20*time.Millisecond // 10% jitter slack
	if elapsed < min {
		t.Fatalf("two 100ms server-lock holders finished in %v, want >= %v (server lock did not serialize)",
			elapsed, min)
	}
}

// TestWithServerLock_NilFn verifies the nil-fn guard.
func TestWithServerLock_NilFn(t *testing.T) {
	if err := WithServerLock(nil); err == nil {
		t.Fatal("WithServerLock(nil) should return an error")
	}
}

// TestWithVendorsConfigLock_SerializesGlobally verifies the global vendors.toml
// lock serializes ALL callers — the guarantee add/edit/remove need so two
// concurrent load→mutate→save cycles don't clobber each other. Two concurrent
// holders must run back-to-back, not in parallel.
func TestWithVendorsConfigLock_SerializesGlobally(t *testing.T) {
	isolateHome(t)
	const hold = 100 * time.Millisecond

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := WithVendorsConfigLock(func() error {
				time.Sleep(hold)
				return nil
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("WithVendorsConfigLock: %v", err)
	}

	elapsed := time.Since(start)
	min := 2*hold - 20*time.Millisecond // 10% jitter slack
	if elapsed < min {
		t.Fatalf("two 100ms vendors-config-lock holders finished in %v, want >= %v (lock did not serialize)",
			elapsed, min)
	}
}

// TestWithVendorsConfigLock_CreatesLockFileInConfigDir verifies the lock file
// lives next to vendors.toml (ConfigDir), honoring XDG_CONFIG_HOME, at 0600.
func TestWithVendorsConfigLock_CreatesLockFileInConfigDir(t *testing.T) {
	home := isolateHome(t)
	t.Setenv("XDG_CONFIG_HOME", "") // force the $HOME/.config fallback
	if err := WithVendorsConfigLock(func() error { return nil }); err != nil {
		t.Fatalf("WithVendorsConfigLock: %v", err)
	}
	path := filepath.Join(home, ".config", "cc-fleet", vendorsLockBasename)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	// NTFS reports 0666; the 0600 contract is unix-only.
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("lock file mode = %o, want 0600", got)
	}
}

// TestWithVendorsConfigLock_PropagatesFnError + nil-fn guard.
func TestWithVendorsConfigLock_PropagatesFnError(t *testing.T) {
	isolateHome(t)
	sentinel := errors.New("boom")
	if err := WithVendorsConfigLock(func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wraps sentinel", err)
	}
}

func TestWithVendorsConfigLock_NilFn(t *testing.T) {
	if err := WithVendorsConfigLock(nil); err == nil {
		t.Fatal("WithVendorsConfigLock(nil) should return an error")
	}
}

// ---------------------------------------------------------------------------
// Cross-process serialization (real flock, not goroutines)
//
// The goroutine tests above can be satisfied by an in-process sync.Mutex; they
// would NOT catch a regression that swapped flock for a mutex, because a mutex
// gives ZERO exclusion across separate OS processes. These tests re-exec the
// test binary N times so N real processes each take the lock and run one
// read-increment-write of a shared counter file inside the critical section.
// Real cross-process exclusion lands the counter at exactly N; any torn /
// unserialized write loses an increment and leaves it < N. Modeled on
// secrets.TestNextRoundRobinIndex_ConcurrentProcesses.
// ---------------------------------------------------------------------------

const lockChildEnv = "CCF_LOCK_CHILD"
const lockCounterEnv = "CCF_LOCK_COUNTER"

// bumpCounter does one read-modify-write of the integer counter at path. The
// short sleep widens the read→write window so an unserialized (no-lock /
// in-process-mutex-only) run reliably loses increments; under a real flock the
// critical section is exclusive and no increment is lost.
func bumpCounter(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	n := 0
	if s := strings.TrimSpace(string(data)); s != "" {
		if n, err = strconv.Atoi(s); err != nil {
			return fmt.Errorf("parse counter: %w", err)
		}
	}
	time.Sleep(10 * time.Millisecond)
	return os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600)
}

// lockChildBump is the env-gated child body: take the supplied lock, do one
// counter increment inside it, then exit (0 on success, 1 on error so the
// parent's CombinedOutput surfaces the failure). It never returns.
func lockChildBump(withLock func(func() error) error) {
	if err := withLock(func() error { return bumpCounter(os.Getenv(lockCounterEnv)) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// spawnLockChildren re-execs this test binary n times (each running only
// testName, which detects lockChildEnv and runs lockChildBump), all targeting
// the same counter file, and fails the test if any child errors. HOME / XDG are
// inherited via os.Environ() so the children resolve the SAME lock path as the
// parent's isolated home.
func spawnLockChildren(t *testing.T, testName, counter string, n int) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
			cmd.Env = append(os.Environ(), lockChildEnv+"=1", lockCounterEnv+"="+counter)
			if out, err := cmd.CombinedOutput(); err != nil {
				errCh <- fmt.Errorf("child lock holder failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
}

func readLockCounter(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counter: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse counter %q: %v", data, err)
	}
	return n
}

// TestWithTeamLock_SerializesAcrossProcesses proves WithTeamLock is a real
// cross-PROCESS lock: N separate processes each increment a shared counter
// inside the lock; the count must be exactly N (no lost update). An in-process
// mutex would let the children race and the counter would land below N.
func TestWithTeamLock_SerializesAcrossProcesses(t *testing.T) {
	if os.Getenv(lockChildEnv) == "1" {
		lockChildBump(func(fn func() error) error { return WithTeamLock("crossproc", fn) })
		return // unreachable: lockChildBump exits
	}

	isolateHome(t)
	counter := filepath.Join(t.TempDir(), "counter")
	const N = 20
	spawnLockChildren(t, "TestWithTeamLock_SerializesAcrossProcesses", counter, N)

	if got := readLockCounter(t, counter); got != N {
		t.Fatalf("counter = %d after %d cross-process WithTeamLock holders, want %d (lost update => no cross-process exclusion)", got, N, N)
	}
}

// TestWithServerLock_SerializesAcrossProcesses is the WithServerLock variant:
// the global tmux-server lock must also serialize across real processes (the
// split/layout race is multi-process by nature). Same counter contract.
func TestWithServerLock_SerializesAcrossProcesses(t *testing.T) {
	if os.Getenv(lockChildEnv) == "1" {
		lockChildBump(WithServerLock)
		return // unreachable: lockChildBump exits
	}

	isolateHome(t)
	counter := filepath.Join(t.TempDir(), "counter")
	const N = 20
	spawnLockChildren(t, "TestWithServerLock_SerializesAcrossProcesses", counter, N)

	if got := readLockCounter(t, counter); got != N {
		t.Fatalf("counter = %d after %d cross-process WithServerLock holders, want %d (lost update => no cross-process exclusion)", got, N, N)
	}
}
