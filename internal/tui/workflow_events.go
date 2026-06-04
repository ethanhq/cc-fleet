package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// eventLine mirrors one line of a run's live-event channel (runs/<id>.events,
// append-only JSONL). The board only READS/tails this stream — it never writes
// it. The JSON tags match the engine's eventRecord; the stream NEVER carries
// prompt/answer text (those live in the leaf io files), so nothing here can leak
// a vendor reply.
type eventLine struct {
	Seq     int64  `json:"seq"`
	Kind    string `json:"kind"`   // phase | log | leaf | group-open | group-close
	Status  string `json:"status"` // leaf: launch | done | failed | cached
	Phase   string `json:"phase"`
	Label   string `json:"label"`
	Vendor  string `json:"vendor"`
	Model   string `json:"model"`
	GroupID string `json:"group_id"`
	GroupTy string `json:"group_type"`
	Msg     string `json:"msg"`
}

// maxLogLines bounds the per-run live-log ring (most-recent lines kept).
const maxLogLines = 200

// runTail is the incremental-tail bookkeeping for one run's events file: the byte
// offset already consumed and a torn trailing partial line carried to next read.
type runTail struct {
	offset  int64
	partial string
}

// tailEvents reads only the bytes appended to path since prev.offset, splits the
// complete lines (carrying a torn trailing partial across reads), unmarshals each,
// and returns the parsed events, the advanced tail, and a reset flag. reset is true
// when the file SHRANK below prev.offset — the engine truncates runs/<id>.events at
// the start of every (re)run (incl. --resume), so a shrink means the prior history
// is gone and the caller must REPLACE (not append) that run's accumulated events +
// log. A missing file or a read error degrades to no new events and the prior tail,
// so a malformed/absent stream never crashes the board. It is a thin IO wrapper
// around parseEventLines (the pure, table-tested core).
func tailEvents(path string, prev runTail) (lines []eventLine, next runTail, reset bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, prev, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, prev, false
	}
	size := info.Size()
	off := prev.offset
	if off > size {
		// The file shrank (truncated by a (re)run): restart from the top and tell the
		// caller to discard the now-stale accumulated history.
		off = 0
		prev.partial = ""
		reset = true
	}
	if off == size {
		return nil, runTail{offset: size, partial: prev.partial}, reset
	}
	if _, err := f.Seek(off, 0); err != nil {
		return nil, prev, false
	}
	buf := make([]byte, size-off)
	n, err := f.Read(buf)
	if err != nil && n == 0 {
		return nil, prev, false
	}
	parsed, partial := parseEventLines(prev.partial + string(buf[:n]))
	return parsed, runTail{offset: off + int64(n), partial: partial}, reset
}

// parseEventLines splits chunk into complete newline-terminated lines, unmarshals
// each into an eventLine (silently skipping a line that doesn't parse), and returns
// the parsed events plus the trailing partial (text after the last newline) to
// carry into the next read. Pure: table-tested independently of the filesystem.
func parseEventLines(chunk string) ([]eventLine, string) {
	var out []eventLine
	for {
		i := strings.IndexByte(chunk, '\n')
		if i < 0 {
			break
		}
		line := chunk[:i]
		chunk = chunk[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev eventLine
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, chunk
}

// renderLogLine formats one event for the flowing live-log pane. Every opaque
// string (phase / label / vendor / model / msg) is CleanTitle-scrubbed before it
// reaches the terminal. A leaf carries its status + label + vendor/model; a phase
// or log carries its narrator msg; a group bracket shows its type.
func renderLogLine(ev eventLine) string {
	clean := sessiontitle.CleanTitle
	switch ev.Kind {
	case "leaf":
		s := "  leaf " + clean(ev.Status) + " · " + clean(ev.Label)
		if ev.Vendor != "" || ev.Model != "" {
			s += " (" + clean(ev.Vendor) + "/" + clean(ev.Model) + ")"
		}
		return s
	case "phase":
		s := "phase " + clean(ev.Phase)
		if ev.Msg != "" {
			s += " — " + clean(ev.Msg)
		}
		return s
	case "group-open":
		return "  ▸ " + clean(ev.GroupTy)
	case "group-close":
		return "  ◂ end"
	default: // log + anything else
		if ev.Msg != "" {
			return "  " + clean(ev.Msg)
		}
		return "  " + clean(ev.Kind)
	}
}

// appendLog pushes rendered lines onto a bounded ring, dropping the oldest when it
// exceeds maxLogLines. The ring is the most-recent rendered log across all runs.
func appendLog(ring []string, evs []eventLine) []string {
	for _, ev := range evs {
		ring = append(ring, renderLogLine(ev))
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
func buildDAG(evs []eventLine) []*dagNode {
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
