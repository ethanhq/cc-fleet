package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// EventRecord is one line of a run's live-event channel (runs/<id>.events). It is PURE
// OBSERVABILITY: `cc-fleet workflow watch` tails it for a scrubbed live status stream —
// the engine NEVER reads it back and it NEVER feeds journalKey, so it cannot perturb
// resume determinism (the load-bearing rule). It is key-safe BY CONSTRUCTION: there is
// no prompt or answer field, so a vendor reply (and the never-present vendor key) cannot
// be written here; a leaf's prompt/answer live in their own 0600 io files. Msg is
// author-supplied script text (phase title / log line), never vendor output.
type EventRecord struct {
	Seq    int64  `json:"seq"`
	Kind   string `json:"kind"`             // phase | log | leaf | group-open | group-close
	Status string `json:"status,omitempty"` // leaf: launch | done | failed | cached | held | stopped
	Phase  string `json:"phase,omitempty"`
	Label  string `json:"label,omitempty"`
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	// Group fields: a parallel/pipeline/workflow group's id and (on group-open) its kind.
	// `workflow watch` brackets the group by seq order (open…close), so no explicit parent
	// id is needed. Empty for plain leaves.
	GroupID string `json:"group_id,omitempty"`
	GroupTy string `json:"group_type,omitempty"`
	Msg     string `json:"msg,omitempty"` // phase title / log narrator line
}

// eventWriter appends a run's live events. It mirrors journal.go: a single
// open/write/close per line (no shared fd, no buffering), MkdirAll-recreate-safe,
// 0600, best-effort, nil-receiver-safe. ALL emits run on the engine loop (builtins emit
// directly; a leaf goroutine's launch/done/failed events ride its posted callbacks), so
// the seq counter needs no atomic and lines never interleave — the same loop-only
// invariant as the journal.
type eventWriter struct {
	path string
	seq  int64
}

func newEventWriter(path string) *eventWriter { return &eventWriter{path: path} }

// emit stamps the next seq and appends one JSON line. Loop-held callers only; nil-safe.
func (w *eventWriter) emit(rec EventRecord) {
	if w == nil {
		return
	}
	w.seq++
	rec.Seq = w.seq
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(w.path), 0o700)
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}
