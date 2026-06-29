package sessiontitle

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ethanhq/cc-fleet/internal/homedir"
)

// maxCodexTitleRunes caps a stored codex title; the board truncates further for display — this
// only avoids holding a whole prompt in memory.
const maxCodexTitleRunes = 120

// codexTitleCache memoizes successfully-resolved codex thread titles (a first user prompt is
// fixed once recorded). A miss is NOT cached, so a just-started session whose rollout has no
// prompt yet is retried on later refreshes.
var (
	codexTitleMu    sync.Mutex
	codexTitleCache = map[string]string{}
)

// ResolveCodexTitles maps each "codex:<thread>" launcher id to the first real user prompt of
// its Codex rollout — the same first_user_message Codex's `resume` picker shows. Best-effort:
// an id whose rollout is missing or unreadable is omitted, and the caller falls back to the id.
func ResolveCodexTitles(ids []string) map[string]string {
	out := map[string]string{}
	seen := map[string]struct{}{}
	for _, id := range ids {
		thread, ok := strings.CutPrefix(id, "codex:")
		if !ok || thread == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if t := codexThreadTitle(thread); t != "" {
			out[id] = t
		}
	}
	return out
}

func codexThreadTitle(thread string) string {
	codexTitleMu.Lock()
	t, cached := codexTitleCache[thread]
	codexTitleMu.Unlock()
	if cached {
		return t
	}
	t = readCodexFirstPrompt(thread)
	if t != "" { // never cache a miss — a just-started session's rollout may not hold a prompt yet
		codexTitleMu.Lock()
		codexTitleCache[thread] = t
		codexTitleMu.Unlock()
	}
	return t
}

// readCodexFirstPrompt finds the thread's rollout under ~/.codex/sessions/YYYY/MM/DD and returns
// its first real user prompt (skipping the developer/AGENTS.md/environment preamble that Codex's
// first_user_message also skips), whitespace-collapsed and rune-capped. "" on any failure.
func readCodexFirstPrompt(thread string) string {
	if !safeThread(thread) { // a uuid only — no glob metacharacters / path separators reach the pattern
		return ""
	}
	home, err := homedir.Home()
	if err != nil {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(matches) == 0 {
		return ""
	}
	return firstUserPrompt(matches[0])
}

func firstUserPrompt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)          // a rollout line can carry a large message
	for scanned := 0; sc.Scan() && scanned < 200; scanned++ { // the first real prompt is early
		var rec struct {
			Payload struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Payload.Role != "user" {
			continue
		}
		var text string
		for _, c := range rec.Payload.Content {
			text += c.Text
		}
		if text = strings.Join(strings.Fields(text), " "); text == "" || isCodexPreamble(text) {
			continue
		}
		if r := []rune(text); len(r) > maxCodexTitleRunes {
			text = string(r[:maxCodexTitleRunes])
		}
		return text
	}
	return ""
}

// safeThread reports whether thread is a canonical uuid (8-4-4-4-12 hex with dashes) — codex
// thread ids always are. Validating the full SHAPE (not just the character set) before it
// reaches the glob keeps a malformed CODEX_THREAD_ID from broadening "rollout-*-<thread>.jsonl"
// into an unrelated match, and bars any glob metacharacter / path separator.
func safeThread(thread string) bool {
	if len(thread) != 36 {
		return false
	}
	for i := 0; i < len(thread); i++ {
		c := thread[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// isCodexPreamble reports whether a user-role message is one of Codex's injected preambles
// (AGENTS.md / environment / instructions) rather than a human prompt.
func isCodexPreamble(text string) bool {
	for _, marker := range []string{"# AGENTS.md", "<INSTRUCTIONS>", "<environment_context>", "<user_instructions>", "<permissions instructions>"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
