package sessiontitle

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// agentMarker is the minimal shape of a transcript line carrying the teammate
// identity native Claude Code stamps on a team agent's session. A teammate's
// own transcript records teamName + agentName on its lines from session start;
// the lead's transcript records teamName only, so it never matches.
type agentMarker struct {
	TeamName  string `json:"teamName"`
	AgentName string `json:"agentName"`
}

// agentTranscriptMatches reports whether the transcript at path records the
// (team, agent) markers within its head chunk.
func agentTranscriptMatches(path, team, agent string) bool {
	if team == "" || agent == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() || st.Size() <= 0 {
		return false
	}
	n := minInt64(st.Size(), headReadBytes)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, 0); err != nil {
		return false
	}
	for _, line := range strings.Split(string(buf), "\n") {
		if !strings.Contains(line, `"agentName"`) {
			continue
		}
		var e agentMarker
		if json.Unmarshal([]byte(line), &e) == nil && e.TeamName == team && e.AgentName == agent {
			return true
		}
	}
	return false
}

// FindAgentTranscript locates a teammate's own session transcript: the newest
// *.jsonl under cwd's project directory whose head records the (team, agent)
// markers. cwd is the lead session's recorded working directory — the spawn
// cwd, which is where native Claude Code files the teammate's transcript too.
// A live teammate's transcript keeps being appended, so candidates modified
// before notBefore (its spawn time) are skipped — that prunes a prior
// same-named teammate's transcript and bounds the scan; a zero notBefore disables the filter.
func FindAgentTranscript(cwd, team, agent string, notBefore time.Time) (string, bool) {
	if cwd == "" || team == "" || agent == "" {
		return "", false
	}
	root := claudeConfigDir()
	if root == "" {
		return "", false
	}
	dir := filepath.Join(root, "projects", sanitizePath(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	type cand struct {
		path  string
		mtime time.Time
	}
	var cands []cand
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().Before(notBefore) {
			continue
		}
		cands = append(cands, cand{path: filepath.Join(dir, e.Name()), mtime: info.ModTime()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mtime.After(cands[j].mtime) })
	for _, c := range cands {
		if agentTranscriptMatches(c.path, team, agent) {
			return c.path, true
		}
	}
	return "", false
}
