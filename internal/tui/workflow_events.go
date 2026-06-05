package tui

import (
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/workflow"
)

// The run event-stream shape, parser, renderer, and tailer live in internal/workflow (the
// owner of the on-disk format): workflow.EventRecord, workflow.RenderEventLine, and the
// value-threaded workflow.TailEvents the board calls in loadWorkflows. This file holds only
// the board-specific projections of that stream — the bounded log ring, the DAG tree, and
// the per-leaf metrics + drill-in io.

// maxLogLines bounds the per-run live-log ring (most-recent lines kept).
const maxLogLines = 200

// appendLog pushes rendered lines onto a bounded ring, dropping the oldest when it
// exceeds maxLogLines. The ring is the most-recent rendered log across all runs.
func appendLog(ring []string, evs []workflow.EventRecord) []string {
	for _, ev := range evs {
		ring = append(ring, workflow.RenderEventLine(ev))
	}
	if len(ring) > maxLogLines {
		ring = ring[len(ring)-maxLogLines:]
	}
	return ring
}

// rebuildLog re-renders the bounded log ring from the accumulated per-run events,
// in manifest list order (matching loadWorkflows' emission order). Used after a
// truncate so the ring no longer carries a reset run's stale lines.
func (m Model) rebuildLog() []string {
	var ring []string
	for _, r := range m.workflowRuns {
		ring = appendLog(ring, m.wfEvents[r.RunID])
	}
	return ring
}

// anyTrue reports whether any value in the map is true.
func anyTrue(m map[string]bool) bool {
	for _, v := range m {
		if v {
			return true
		}
	}
	return false
}

// dagNode is one node of a run's reconstructed structure tree: a group
// (parallel/pipeline/workflow) holding child nodes, or a leaf. Leaves carry the
// event's label/vendor/model/status so the DAG view can render them like tree rows.
type dagNode struct {
	group    bool
	groupTy  string // "parallel" | "pipeline" | "workflow" (group nodes only)
	label    string // leaf label (leaf nodes only)
	vendor   string
	model    string
	status   string
	children []*dagNode
}

// buildDAG reconstructs a run's structure tree from its events by seq-nesting:
// group-open pushes a group node, group-close pops it, and a leaf attaches to the
// currently-open group (or the root when none is open). Nested groups nest by
// bracket order — no parent id is threaded. An unmatched close is ignored (degrades
// gracefully). Returns the root's children, or nil when the run has no group events
// (the caller then falls back to the flat phase→agent tree).
func buildDAG(evs []workflow.EventRecord) []*dagNode {
	hasGroup := false
	for _, ev := range evs {
		if ev.Kind == "group-open" {
			hasGroup = true
			break
		}
	}
	if !hasGroup {
		return nil
	}
	root := &dagNode{group: true}
	stack := []*dagNode{root}
	top := func() *dagNode { return stack[len(stack)-1] }
	for _, ev := range evs {
		switch ev.Kind {
		case "group-open":
			n := &dagNode{group: true, groupTy: ev.GroupTy}
			top().children = append(top().children, n)
			stack = append(stack, n)
		case "group-close":
			if len(stack) > 1 { // never pop the root
				stack = stack[:len(stack)-1]
			}
		case "leaf":
			top().children = append(top().children, &dagNode{
				label: ev.Label, vendor: ev.Vendor, model: ev.Model, status: ev.Status,
			})
		}
	}
	return root.children
}

// jobMetrics distills the per-leaf token/cost/turn columns from a job Result. A nil
// Usage degrades to zeros. Kept separate from rendering so the totals line can sum
// the same numbers.
type jobMetrics struct {
	inTok, outTok, cacheTok int
	cost                    float64
	turns                   int
}

func metricsOf(j subagent.Result) jobMetrics {
	m := jobMetrics{cost: j.CostUSD, turns: j.NumTurns}
	if j.Usage != nil {
		m.inTok = j.Usage.InputTokens
		m.outTok = j.Usage.OutputTokens
		m.cacheTok = j.Usage.CacheReadInputTokens
	}
	return m
}

// readLeafIO reads a leaf's prompt/answer side files at
// <ConfigDir>/subagent-jobs/<jobID>.prompt / .answer (0600, present only when the
// run persisted io). It returns the raw text plus whether EITHER file was present;
// an absent/invalid jobID, an unresolvable config dir, or a missing file degrades
// to ("", "", false) so the drill-in card shows the not-persisted note rather than
// crashing. The answer text returned here reaches only the drill-in card.
//
// jobID becomes a path component, so it MUST be validated first: a malformed cached
// Result.JobID (e.g. containing "../") could otherwise read outside subagent-jobs.
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
