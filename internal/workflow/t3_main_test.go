//go:build unix

package workflow

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
)

// TestMain lets a T3 test drive REAL detached engines. launchDetached re-execs os.Executable() — here the
// TEST binary — as `workflow run <script> --foreground --run-id <id> ...`; intercepting that argv and
// running the engine (Execute) instead of the test suite makes a detached "child" a genuine second process
// running the real engine code path. Any other invocation runs the tests normally. When PID_LOG is set, the
// child appends its pid so a test can assert how many engines stayed alive (the one-engine-per-run check).
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "workflow" && os.Args[2] == "run" {
		os.Exit(runEngineChild(os.Args[3:]))
	}
	os.Exit(m.Run())
}

func runEngineChild(args []string) int {
	var script, runID string
	opts := Options{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--foreground":
		case "--run-id":
			i++
			runID = args[i]
			opts.RunID = runID
		case "--max-concurrency":
			i++
			opts.Concurrency, _ = strconv.Atoi(args[i])
		case "--args-json":
			i++
			opts.ArgsJSON = args[i]
		case "--no-persist-io":
			opts.NoPersistIO = true
		case "--budget-usd":
			i++
			opts.BudgetUSD, _ = strconv.ParseFloat(args[i], 64)
		default:
			if script == "" && args[i] != "" && args[i][0] != '-' {
				script = args[i]
			}
		}
	}
	if pidLog := os.Getenv("PID_LOG"); pidLog != "" {
		if f, err := os.OpenFile(pidLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
		}
	}
	if err := Execute(context.Background(), script, runID, opts); err != nil {
		return 1
	}
	return 0
}
