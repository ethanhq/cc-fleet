//go:build unix

package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// blockingFakeClaude blocks until the test removes $GATE_FILE, so a workflow leaf stays in-flight and its
// engine stays live — long enough to fire a restart storm against it.
const blockingFakeClaude = `#!/bin/sh
cat > /dev/null
while [ -f "$GATE_FILE" ]; do sleep 0.05; done
printf '{"type":"result","subtype":"success","is_error":false,"result":"ok","num_turns":1,"total_cost_usd":0.001,"usage":{"input_tokens":5,"output_tokens":5}}'
`

// TestT3_RestartStormOneEngine is the T3 real-restart e2e: a detached engine runs a leaf that BLOCKS (so
// the engine stays live), then N concurrent Restart calls hit the same run. The per-run execution lock must
// serialize them so exactly ONE engine survives — no multi-engine pile-up. Real detached engines are spawned
// via the TestMain re-dispatch (each appends its pid to $PID_LOG).
func TestT3_RestartStormOneEngine(t *testing.T) {
	env := newE2EEnv(t)
	gate := filepath.Join(env.home, "gate")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pidLog := filepath.Join(env.home, "pids")
	t.Setenv("GATE_FILE", gate)
	t.Setenv("PID_LOG", pidLog)
	if err := os.WriteFile(env.fakePath, []byte(blockingFakeClaude), 0o755); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(env.home, "block.star")
	body := "meta = {\"name\": \"t3\", \"description\": \"d\"}\nphase(\"p\")\nagent(\"block\", vendor=\"fake\", label=\"b\")\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Launch a detached run; its single leaf blocks on the gate, so the engine stays live.
	runID, err := Launch(context.Background(), script, Options{NoPersistIO: true}, false)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(gate); _, _ = subagent.StopRun(runID) }) // unblock + reap on exit
	if !waitEngineLive(t, runID, 5*time.Second) {
		t.Fatal("the initial detached engine never registered as live")
	}
	run0, err := subagent.ReadRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	initialPID := run0.EnginePID
	if initialPID <= 0 {
		t.Fatalf("the initial engine pid should be recorded, got %d", initialPID)
	}

	// Restart storm: N concurrent whole-run restarts. The per-run lock must serialize them — each fully
	// kills the previous engine and registers a new one before releasing, so they never pile up.
	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := Restart(context.Background(), runID, ""); e != nil {
				mu.Lock()
				errs = append(errs, e)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("every serialized restart must succeed, got %d error(s); first: %v", len(errs), errs[0])
	}

	run, err := subagent.ReadRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	// A restart ACTUALLY replaced the engine (proves the storm was not a no-op): a new, live pid.
	if !subagent.EngineAlive(run) {
		t.Fatalf("after the storm the run should still have ONE live engine (manifest pid %d)", run.EnginePID)
	}
	if run.EnginePID == initialPID {
		t.Fatalf("the restart storm did not replace the engine — pid is still the initial %d", initialPID)
	}
	// The pid side channel is populated (initial + restart engines) and names the survivor.
	pids := readPIDs(pidLog)
	if len(pids) < 2 || !containsInt(pids, run.EnginePID) {
		t.Fatalf("PID_LOG %v must record the initial + restart engines and include the survivor %d", pids, run.EnginePID)
	}
	// And NO engine besides the survivor is still alive — i.e. no multi-engine pile-up.
	stray := 0
	for _, pid := range pids {
		if pid != run.EnginePID && syscall.Kill(pid, 0) == nil {
			stray++
		}
	}
	if stray != 0 {
		t.Fatalf("multi-engine pile-up: %d engine pid(s) besides the survivor (%d) are still alive", stray, run.EnginePID)
	}
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func waitEngineLive(t *testing.T, runID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if run, err := subagent.ReadRun(runID); err == nil && subagent.EngineAlive(run) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func readPIDs(path string) []int {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if p, perr := strconv.Atoi(strings.TrimSpace(line)); perr == nil && p > 0 {
			pids = append(pids, p)
		}
	}
	return pids
}
