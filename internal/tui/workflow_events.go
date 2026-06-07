package tui

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// This file holds the board's per-leaf data projections: the live tool-call + token activity read
// from a leaf's <jobID>.activity sidecar, and the prompt/answer io for the focused agent's inline
// detail. The on-disk event-stream type / parser / renderer / tailer live in internal/workflow; the
// board renders per-leaf activity + the final Result metrics, not the raw stream.

// activitySnapshot is one leaf's live activity, read from its <jobID>.activity sidecar: the ordered
// tool-call signatures ("Tool(arg)") and the latest running token snapshot. It is what makes the
// board's per-agent token + tool counts climb WHILE a sync leaf runs (before its final Result caches).
type activitySnapshot struct {
	sigs          []string // tool-call signatures in arrival order
	inTok, outTok int
	hasUsage      bool
}

// toolCount is the number of tool calls the leaf has made so far.
func (s activitySnapshot) toolCount() int { return len(s.sigs) }

// lastSigs returns the most-recent n signatures (fewer if the leaf made fewer).
func (s activitySnapshot) lastSigs(n int) []string {
	if len(s.sigs) <= n {
		return s.sigs
	}
	return s.sigs[len(s.sigs)-n:]
}

// readLeafActivity reads a leaf's <jobID>.activity sidecar (NDJSON, 0600, present only when the run
// streamed activity) into a snapshot. The jobID becomes a path component, so it MUST be validated
// first (a malformed cached Result.JobID could otherwise read outside subagent-jobs). An absent/
// invalid id or missing file degrades to (zero, false). The tool args are model content already
// masked at write; the board CleanTitle-scrubs them at render — never the answer, never the key.
func readLeafActivity(jobID string) (activitySnapshot, bool) {
	if err := ids.ValidateJobID(jobID); err != nil {
		return activitySnapshot{}, false
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return activitySnapshot{}, false
	}
	f, err := os.Open(filepath.Join(dir, "subagent-jobs", jobID+".activity"))
	if err != nil {
		return activitySnapshot{}, false
	}
	defer f.Close()
	var snap activitySnapshot
	present := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r struct {
			Kind string `json:"kind"`
			Tool string `json:"tool"`
			Arg  string `json:"arg"`
			In   int    `json:"in"`
			Out  int    `json:"out"`
		}
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		present = true
		switch r.Kind {
		case "tool":
			sig := r.Tool
			if r.Arg != "" {
				sig += "(" + r.Arg + ")"
			}
			snap.sigs = append(snap.sigs, sig)
		case "usage":
			snap.inTok, snap.outTok, snap.hasUsage = r.In, r.Out, true
		}
	}
	return snap, present
}

// readLeafIO reads a leaf's prompt/answer side files at
// <ConfigDir>/subagent-jobs/<jobID>.prompt / .answer (0600, present only when the run persisted io).
// It returns the raw text plus whether EITHER file was present; an absent/invalid jobID, an
// unresolvable config dir, or a missing file degrades to ("", "", false) so the inline detail shows
// the not-persisted note rather than crashing. The answer text returned here reaches only the
// focused agent's inline detail pane.
//
// jobID becomes a path component, so it MUST be validated first: a malformed cached Result.JobID
// (e.g. containing "../") could otherwise read outside subagent-jobs.
func readLeafIO(jobID string) (prompt, answer string, present bool) {
	if err := ids.ValidateJobID(jobID); err != nil {
		return "", "", false
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return "", "", false
	}
	base := filepath.Join(dir, "subagent-jobs", jobID)
	if b, rerr := os.ReadFile(base + ".prompt"); rerr == nil {
		prompt = string(b)
		present = true
	}
	if b, rerr := os.ReadFile(base + ".answer"); rerr == nil {
		answer = string(b)
		present = true
	}
	return prompt, answer, present
}
