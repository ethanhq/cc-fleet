package codexproxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// countJobWorkers is the windows liveWorkers source: it counts subagent job metas
// that are running, bound to the port, and live. The predicate is platform-neutral,
// so this runs on any OS with a fake alive func and a temp jobs dir.
func TestCountJobWorkers(t *testing.T) {
	dir := t.TempDir()
	write := func(id, status string, pid, proxyPort int, procStart string) {
		body := fmt.Sprintf(`{"job_id":%q,"status":%q,"pid":%d,"proc_start":%q,"proxy_port":%d}`,
			id, status, pid, procStart, proxyPort)
		if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	const port = 17222
	write("a", "running", 100, port, "tok-a") // counts
	write("b", "running", 101, port, "tok-b") // counts
	write("c", "done", 102, port, "tok-c")    // not running
	write("d", "running", 103, 9999, "tok-d") // different port
	write("e", "running", 0, port, "")        // held leaf (pid 0) -> dead
	write("f", "running", 104, port, "stale") // alive() says no (recycled pid)

	// A result-cache file must be skipped even if it looks running.
	if err := os.WriteFile(filepath.Join(dir, "g.result.json"),
		[]byte(fmt.Sprintf(`{"status":"running","proxy_port":%d,"pid":105,"proc_start":"tok-g"}`, port)), 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-json sibling and a subdir are ignored.
	if err := os.WriteFile(filepath.Join(dir, "a.out"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}

	alive := func(pid int, procStart string) bool {
		return pid > 0 && procStart != "" && procStart != "stale"
	}
	if got := countJobWorkers(dir, port, alive); got != 2 {
		t.Fatalf("countJobWorkers = %d, want 2", got)
	}

	// A missing dir counts zero (never -1: the idle timer still gates exit).
	if got := countJobWorkers(filepath.Join(dir, "nope"), port, alive); got != 0 {
		t.Fatalf("missing dir = %d, want 0", got)
	}
}
