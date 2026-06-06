package workflow

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// journal is a run's append-only content-hash cache of completed leaf results — the
// engine of cross-invocation resume. On `workflow run --resume <id>` (or any re-run
// reusing the id) a leaf whose content key is journaled returns its cached result
// WITHOUT a vendor exec; only un-journaled leaves (new, edited, or failed-last-time)
// run. Because Starlark bans the clock/PRNG, the same script+args produce the same
// keys, so an unchanged re-run is ~100% cache hits and a killed run resumes by
// replaying the leaves that finished before the kill.
//
// Every access is under the engine GIL — lookup before a leaf's exec, append after
// runBlocking re-locks — so `seen` needs no separate lock and appends never interleave.
// All methods are nil-receiver-safe: an engine constructed without a journal (the leaf
// unit tests) simply never caches.
//
// `seen` is the PRIOR-RUN snapshot, loaded once at engine start and NOT mutated by
// append. So replay serves only results journaled by an EARLIER invocation; agent() calls
// within the SAME run never memoize against each other.
//
// Each key maps to an ORDERED QUEUE of results (in journaled = original execution order),
// and a lookup CONSUMES one entry. This is what makes duplicate-key leaves correct: a
// script that calls agent() with the same vendor/model/prompt/schema N times (e.g. a
// loop-until-dry probing the same prompt) produces N entries under one key. A run killed
// after K of N completed journals K entries, so on resume the first K calls each pop one
// cached result and calls K+1..N find the queue empty and RE-RUN — only the unrun work
// runs. An unchanged full re-run pops all N (100% hits). A single-key map would instead
// serve every one of the N calls the one surviving result and skip the unrun tail.
type journal struct {
	path string
	seen map[string][]string // prior-run content-hash key → FIFO queue of cached results
}

// journalEntry is one JSONL line: a successfully-completed leaf's content key and its
// raw answer string. Failed/partial leaves are never written, so resume re-runs them.
// The raw answer string is stored uniformly (schema leaves re-decode+re-validate it on
// replay — deterministic, no vendor exec). The vendor key never enters a result, so the
// journal carries no secret; files are 0600 (the board's content-privacy posture).
type journalEntry struct {
	Key    string `json:"key"`
	Result string `json:"result"`
}

// loadJournal reads an existing journal into memory; a missing/unreadable file yields
// an empty cache (the fresh-run case — the first append creates the file). A torn final
// line from a crash mid-append, or any malformed line, is skipped rather than aborting
// the load, and arbitrarily long answer lines are handled (bufio.Reader, not Scanner).
// Repeated keys accumulate a FIFO queue in journaled (original execution) order.
func loadJournal(path string) *journal {
	j := &journal{path: path, seen: map[string][]string{}}
	f, err := os.Open(path)
	if err != nil {
		return j
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var e journalEntry
			if json.Unmarshal(line, &e) == nil && e.Key != "" {
				// Preserve journaled (= original execution) order per key; lookup
				// consumes one entry per call, so N duplicate-key leaves replay 1:1.
				j.seen[e.Key] = append(j.seen[e.Key], e.Result)
			}
		}
		if err != nil {
			break // io.EOF, or a read fault — stop with whatever loaded
		}
	}
	return j
}

// lookup returns (and CONSUMES) the next cached result for key — FIFO, matching the
// order the leaves originally completed. An empty/absent queue is a miss, so once a
// key's prior-run results are exhausted, further duplicate calls re-run. GIL-held
// callers only (so the pop never races); nil-safe.
func (j *journal) lookup(key string) (string, bool) {
	if j == nil {
		return "", false
	}
	q := j.seen[key]
	if len(q) == 0 {
		return "", false
	}
	j.seen[key] = q[1:]
	return q[0], true
}

// append records a completed leaf by writing one JSONL line (open O_APPEND / write /
// close — each line independently flushed, so a crash leaves a clean prefix). It does
// NOT update `seen`: the in-memory cache is the prior-run snapshot, so a later resume
// (which reloads the file) picks this up, but the CURRENT run's own duplicate calls do
// not memoize against it. GIL-held callers only, so appends never interleave. Best-effort
// like the manifest writes: a write hiccup leaves the result unjournaled (a later resume
// just re-runs that leaf), it never fails the run. Nil-safe.
func (j *journal) append(key, result string) {
	if j == nil {
		return
	}
	data, err := json.Marshal(journalEntry{Key: key, Result: result})
	if err != nil {
		return
	}
	// Recreate the runs dir if absent (matches the engine's recreate-safe manifest
	// writes: a dir a concurrent GC happened to drop is remade on the next write).
	_ = os.MkdirAll(filepath.Dir(j.path), 0o700)
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// removeJournalKey rewrites a run's journal file, dropping EVERY entry whose key matches —
// the journal half of the board's single-leaf restart. A leaf's content key can appear more than once
// (identical agent() calls share it; they are interchangeable by construction), so dropping
// all matching entries re-runs that whole interchangeable set rather than corrupting the
// FIFO queue by removing a positional one. On the next resume the dropped leaf finds no cache
// and re-runs live; content-addressing then re-runs any downstream leaf whose input shifted. The
// "single-leaf" scope is precise only for an already-terminal run: the caller (Restart) stops a live
// engine first, and a stopped engine leaves every in-flight sibling un-journaled, so those re-run too.
//
// A missing journal (nothing cached yet) or an absent key (e.g. a leaf that failed last time,
// never journaled) is a no-op success — the leaf re-runs on resume regardless. The rewrite
// goes through the atomic-write outlet; the CALLER must have confirmed the engine is dead
// first (a live O_APPEND would race this whole-file replace). Returns whether anything was
// dropped. Malformed/torn lines are preserved verbatim (loadJournal skips them on reload).
func removeJournalKey(path, key string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // nothing journaled → the leaf re-runs anyway
		}
		return false, err
	}
	var kept []byte
	removed := false
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			var e journalEntry
			if json.Unmarshal(line, &e) == nil && e.Key == key {
				removed = true // drop this entry
			} else {
				kept = append(kept, line...)
			}
		}
		if rerr != nil {
			break // io.EOF or a read fault — stop with whatever loaded
		}
	}
	f.Close()
	if !removed {
		return false, nil // key absent → no rewrite needed
	}
	if werr := fileutil.AtomicWrite(path, kept, 0o600); werr != nil {
		return false, werr
	}
	return true, nil
}

// journalKey is the content hash of a leaf's result determinant: vendor, model, the
// BASE prompt, the schema JSON, and the isolation mode (a worktree leaf can produce a
// different result than an in-place one). It EXCLUDES display-only fields (label/phase) and
// the caps (timeout/max_budget_usd/max_turns — those only ever yield a failure, which
// is never journaled). model is the EFFECTIVE model: the explicit model= if given, else
// the meta.model fallback (applied in agent() BEFORE this key is computed), else empty —
// in which case the vendor resolves its own default_model at runtime (the caveat below).
// It is keyed as the script determines it, not the vendor-default it later resolves to.
//
// effProfile is the EFFECTIVE slim profile (post-version-gate, resolved in agent() before
// keying) with its resolved tools/skills/mcp. When the effective profile is full ("" or
// "full") the slim fields are NOT folded, so existing saved-run keys stay byte-identical —
// only a slim leaf's key carries the extra shape. Folding the EFFECTIVE (not requested)
// profile means a cross-machine resume whose claude resolves a different effective profile
// recomputes a different key → cache miss + re-run, never a wrong-shape replay. Ambient
// context (CLAUDE.md/gitStatus/host skills listing/MCP config) is intentionally NOT keyed —
// the same ambient-context exclusion the full-profile journal already makes.
//
// Caveat: when BOTH model= and meta.model are omitted the key holds
// the empty string, so a vendor-config change between a run and its resume — editing a
// vendor's default_model or base_url — is NOT captured, and an omitted-model leaf could
// serve a result produced under the old config.
// In practice a resume reuses the run id moments later under stable config; after a
// deliberate vendor-config change, start a fresh run (or the "v1" scheme prefix can be
// bumped). schemaJSON is the deterministic json.encode of the schema (go.starlark.net
// canonicalizes it → stable).
//
// Each field is LENGTH-PREFIXED (8-byte big-endian) rather than separated by a sentinel
// byte: a prompt is an arbitrary Starlark string that may itself contain any byte, so a
// sentinel-only framing (vendor\x00model\x00prompt…) could collide across field
// boundaries; length-prefixing makes the preimage unambiguous for any content.
func journalKey(vendor, model, prompt, schemaJSON, isolation, effProfile string, tools []string, noSkills, mcp bool) string {
	h := sha256.New()
	h.Write([]byte("v1"))
	var n [8]byte
	parts := []string{vendor, model, prompt, schemaJSON, isolation}
	// Fold the slim determinants only for a non-full effective profile, with the same
	// length-prefix discipline — so a full leaf's preimage is byte-identical to today's.
	if effProfile != "" && effProfile != subagent.ProfileFull {
		parts = append(parts,
			effProfile,
			strings.Join(tools, ","),
			"skills="+strconv.FormatBool(!noSkills),
			"mcp="+strconv.FormatBool(mcp),
		)
	}
	for _, part := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
