package codexproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// jobRecord is the subset of internal/subagent's jobMeta the windows liveWorkers
// reads from <ConfigDir>/subagent-jobs/<id>.json. The schema (these field names and
// their meaning) is owned by internal/subagent; codexproxy cannot import that
// package (import cycle), so it decodes the fields it needs locally. A held leaf
// clears PID to 0, which pidAlive rejects — so it counts zero.
type jobRecord struct {
	Status    string `json:"status"`
	PID       int    `json:"pid"`
	ProcStart string `json:"proc_start"`
	ProxyPort int    `json:"proxy_port"`
}

// countJobWorkers counts the subagent jobs in jobsDir that are bound to port and
// still live: status "running", proxy_port == port, and pidAlive(pid, proc_start)
// (the same start-token identity the daemon's own pidAlive uses). A non-running, a
// different-port, or a dead-pid job does not count. A missing dir counts zero. Pure
// over (jobsDir, port, alive), so it is testable on any platform.
func countJobWorkers(jobsDir string, port int, alive func(pid int, procStart string) bool) int {
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		// Meta files only: <id>.json, never the <id>.result.json cache (subagent's
		// own meta scan applies the same filter).
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(jobsDir, name))
		if err != nil {
			continue
		}
		var rec jobRecord
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		if rec.Status == "running" && rec.ProxyPort == port && alive(rec.PID, rec.ProcStart) {
			n++
		}
	}
	return n
}
