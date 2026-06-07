// Package sessiontitle resolves human-readable Claude Code session titles from
// transcript metadata. It is read-only and best-effort: callers fall back to the
// session UUID when no title can be resolved.
package sessiontitle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	headReadBytes = 64 * 1024
	tailReadBytes = 256 * 1024
)

type titleEntry struct {
	Type        string `json:"type"`
	SessionID   string `json:"sessionId"`
	CustomTitle string `json:"customTitle"`
	AITitle     string `json:"aiTitle"`
}

// cwdEntry is the minimal shape of a transcript line that records the
// session's working directory.
type cwdEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

type candidate struct {
	path    string
	mtime   time.Time
	current bool
}

// Meta is one session's resolved display metadata: its /rename (or AI) title
// and the working directory its transcript records. Either field may be empty.
type Meta struct {
	Title string
	Cwd   string
}

// Resolve returns title metadata for ids. custom-title entries written by
// /rename win over ai-title entries; missing or malformed transcripts simply
// omit that session from the returned map.
func Resolve(ids []string) map[string]string {
	out := map[string]string{}
	for id, meta := range ResolveMeta(ids) {
		if meta.Title != "" {
			out[id] = meta.Title
		}
	}
	return out
}

// ResolveMeta returns title + working-directory metadata for ids. A session
// with neither a title nor a cwd is omitted from the returned map.
func ResolveMeta(ids []string) map[string]Meta {
	out := map[string]Meta{}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if meta := LookupMeta(id); meta != (Meta{}) {
			out[id] = meta
		}
	}
	return out
}

// Lookup returns the best display title for one Claude session id.
func Lookup(sessionID string) string {
	return LookupMeta(sessionID).Title
}

// LookupMeta returns the best display title + recorded working directory for
// one Claude session id, merging across transcript candidates first-wins per
// field and stopping once both are known.
func LookupMeta(sessionID string) Meta {
	if sessionID == "" {
		return Meta{}
	}
	var out Meta
	for _, c := range transcriptCandidates(sessionID) {
		got := readTranscriptMeta(c.path, sessionID)
		if out.Title == "" {
			out.Title = got.Title
		}
		if out.Cwd == "" {
			out.Cwd = got.Cwd
		}
		if out.Title != "" && out.Cwd != "" {
			break
		}
	}
	return out
}

func transcriptCandidates(sessionID string) []candidate {
	root := claudeConfigDir()
	if root == "" {
		return nil
	}
	projects := filepath.Join(root, "projects")
	seen := map[string]struct{}{}
	var out []candidate
	add := func(path string, current bool) {
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		st, err := os.Stat(path)
		if err != nil || st.IsDir() {
			return
		}
		out = append(out, candidate{path: path, mtime: st.ModTime(), current: current})
	}

	if cwd, err := os.Getwd(); err == nil {
		add(filepath.Join(projects, sanitizePath(cwd), sessionID+".jsonl"), true)
	}

	entries, err := os.ReadDir(projects)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			add(filepath.Join(projects, e.Name(), sessionID+".jsonl"), false)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].current != out[j].current {
			return out[i].current
		}
		return out[i].mtime.After(out[j].mtime)
	})
	return out
}

func readTranscriptMeta(path, sessionID string) Meta {
	f, err := os.Open(path)
	if err != nil {
		return Meta{}
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.IsDir() || st.Size() <= 0 {
		return Meta{}
	}

	size := st.Size()
	state := titleState{}
	readChunk := func(offset, n int64) {
		if n <= 0 {
			return
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, offset); err != nil {
			return
		}
		state.scan(string(buf), sessionID)
	}

	headLen := minInt64(size, headReadBytes)
	readChunk(0, headLen)

	tailLen := minInt64(size, tailReadBytes)
	// A fixed `size - tailLen` offset can land mid-line, splitting a title line
	// into a dropped head-half and an unparseable tail-half. Seek back to the
	// nearest \n+1 so every tail-chunk line is whole; maxLookback bounds the scan.
	tailStart := size - tailLen
	if tailStart > 0 {
		tailStart = seekToNewline(f, tailStart, 4096)
		tailLen = size - tailStart
	}
	if tailStart > 0 {
		readChunk(tailStart, tailLen)
	}

	title := state.ai
	if state.custom != "" {
		title = state.custom
	}
	return Meta{Title: title, Cwd: state.cwd}
}

// seekToNewline returns the offset just past the nearest preceding '\n' before
// start (so the caller reads forward and gets whole lines). maxLookback caps the
// reverse scan in bytes — beyond that the original start is returned, accepting
// a possibly-partial tail-edge line but preventing unbounded I/O on a corrupt
// single-line file.
//
// Returns 0 on any read error or when start <= 0 — the caller treats 0 as the
// "start of file" sentinel.
func seekToNewline(f *os.File, start int64, maxLookback int64) int64 {
	if start <= 0 {
		return 0
	}
	if maxLookback <= 0 {
		return start
	}
	original := start
	chunk := int64(512)
	if maxLookback < chunk {
		chunk = maxLookback
	}
	remaining := maxLookback
	for remaining > 0 {
		read := chunk
		if read > remaining {
			read = remaining
		}
		readAt := start - read
		if readAt < 0 {
			read += readAt // shrink so we don't read negative
			readAt = 0
		}
		if read <= 0 {
			return 0
		}
		buf := make([]byte, read)
		n, err := f.ReadAt(buf, readAt)
		if err != nil && n == 0 {
			return original
		}
		buf = buf[:n]
		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return readAt + int64(i) + 1
			}
		}
		remaining -= int64(n)
		if readAt == 0 {
			return 0
		}
		start = readAt
	}
	// Looked the full maxLookback distance without finding a newline: fall
	// back to the original start.
	return original
}

type titleState struct {
	custom string
	ai     string
	cwd    string
}

func (s *titleState) scan(chunk, sessionID string) {
	for _, line := range strings.Split(chunk, "\n") {
		// The session's working directory rides on ordinary transcript lines;
		// the first match wins (the head chunk scans before the tail).
		if s.cwd == "" && strings.Contains(line, `"cwd"`) {
			var entry cwdEntry
			if json.Unmarshal([]byte(line), &entry) == nil &&
				entry.SessionID == sessionID && entry.Cwd != "" {
				// Sanitized like the titles: a transcript-supplied cwd must not carry
				// control bytes into display or grouping keys.
				s.cwd = cleanTitle(entry.Cwd)
			}
		}
		if !strings.Contains(line, `"type"`) ||
			(!strings.Contains(line, "custom-title") && !strings.Contains(line, "ai-title")) {
			continue
		}
		var entry titleEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.SessionID != sessionID {
			continue
		}
		switch entry.Type {
		case "custom-title":
			if title := cleanTitle(entry.CustomTitle); title != "" {
				s.custom = title
			}
		case "ai-title":
			if title := cleanTitle(entry.AITitle); title != "" {
				s.ai = title
			}
		}
	}
}

// CleanTitle sanitizes a Claude session title for display in the TUI board
// header. It drops every non-whitespace control rune (unicode.IsControl &&
// !IsSpace — the ESC byte that introduces an ANSI sequence, plus BEL and OSC
// terminators) so a /rename title can't inject escape sequences into the
// terminal. Once ESC is gone, any leftover body (e.g. "[31m") is inert text.
// Whitespace control runes (space/tab/newline/CR) are KEPT so strings.Fields
// collapses them to single spaces rather than gluing words together.
func CleanTitle(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// cleanTitle is the unexported alias delegating to CleanTitle.
func cleanTitle(title string) string {
	return CleanTitle(title)
}

func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home := os.Getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func sanitizePath(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() <= 200 {
		return b.String()
	}
	return b.String()[:200] + "-" + strconv.FormatUint(simpleHash(name), 36)
}

func simpleHash(s string) uint64 {
	var h uint64 = 5381
	for _, r := range s {
		h = ((h << 5) + h) + uint64(r)
	}
	return h
}

func minInt64(a int64, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
