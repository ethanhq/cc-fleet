package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/spawn"
	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// This file holds the teammate detail card's data projections (the sibling of
// workflow_events.go): the teammate's received messages, its output messages, and
// its token usage, all projected from its own session transcript plus the native
// inbox file. Everything read here reaches ONLY the focused teammate's inline
// detail card — never a row, rail, or header beyond integer token counts.

// inboxEntry is one message of a teammate's native inbox
// (~/.claude/teams/<team>/inboxes/<name>.json). The inbox is a delivery queue:
// a consumed message moves into the teammate's transcript and leaves the file,
// so unread entries here are the PENDING backlog.
type inboxEntry struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
}

// readTeammateInbox reads a teammate's inbox file. spawn.InboxPath validates the
// team/name path components, so a malformed discovered name can never escape the
// teams root. An absent or unparseable file degrades to an empty inbox.
func readTeammateInbox(team, name string) []inboxEntry {
	path, err := spawn.InboxPath(team, name)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []inboxEntry
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	return entries
}

// inboxPreview is a message body's one-line display text: native task payloads
// arrive as a JSON blob — best-effort lift their "subject"; anything else shows
// its first line.
func inboxPreview(text string) string {
	var payload struct {
		Subject string `json:"subject"`
	}
	if json.Unmarshal([]byte(text), &payload) == nil && payload.Subject != "" {
		return payload.Subject
	}
	line, _, _ := strings.Cut(text, "\n")
	return line
}

// mateMessage is one message the teammate received — pending (an unread inbox
// entry awaiting delivery) or consumed (replayed from the transcript's
// teammate-message wrapper). ts is RFC3339 (mixed precision across sources —
// parse before comparing).
type mateMessage struct {
	from    string
	summary string // one-line preview: the wrapper's summary attr, else the body's subject/first line
	body    string
	ts      string
	pending bool
}

// mateOutput is one assistant text message from the teammate's transcript.
type mateOutput struct {
	text string
	ts   string
}

// teammateSnapshot is the focused teammate's transcript projection: tool-call
// signatures (the job card's Activity shape), the received messages, the output
// messages, and the token aggregates the session header shows.
type teammateSnapshot struct {
	activity activitySnapshot // sigs only; the token fields stay zero
	msgs     []mateMessage    // received messages, chronological (render newest first)
	outputs  []mateOutput     // assistant texts, chronological (render newest first)
	ctxTok   int              // ↑ peak context: max(input+cache-read) over the transcript
	outTok   int              // ↓ cumulative output: summed output_tokens
}

// transcriptLine is the minimal shape of a transcript line: assistant lines carry
// content BLOCKS + usage; user lines carry a content STRING (the teammate-message
// wrapper). Content stays raw so both shapes decode lazily.
type transcriptLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens    int `json:"input_tokens"`
			OutputTokens   int `json:"output_tokens"`
			CacheReadInput int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// maxTranscriptLine bounds a single transcript line held in memory (large tool
// results inflate user lines); an oversized line is skipped, never ending the scan.
const maxTranscriptLine = 8 * 1024 * 1024

// readTeammateTranscript projects the whole transcript at path in one streaming
// pass: tool_use blocks become "Tool(arg)" signatures, text blocks become output
// messages, teammate-message user lines become received messages, and the usage
// fields aggregate into the ↑ peak-context / ↓ summed-output pair (a full-file
// scan — a bounded tail would under-count the sums). ok=false when the transcript
// is unreadable. The extracted strings are CleanTitle-scrubbed at render, like
// every other board surface.
func readTeammateTranscript(path string) (teammateSnapshot, bool) {
	f, err := os.Open(path)
	if err != nil {
		return teammateSnapshot{}, false
	}
	defer f.Close()
	var snap teammateSnapshot
	// Hand-rolled line loop instead of bufio.Scanner: a Scanner ABORTS on a line over
	// its cap, silently dropping everything after it — here an oversized line (a huge
	// tool result) is drained and skipped, and the scan continues.
	r := bufio.NewReaderSize(f, 64*1024)
	var line []byte
	skipping := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !skipping {
			line = append(line, chunk...)
			if len(line) > maxTranscriptLine {
				skipping = true
				line = line[:0]
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if !skipping && len(line) > 0 {
			scanTranscriptLine(bytes.TrimSuffix(line, []byte("\n")), &snap)
			line = line[:0]
		}
		skipping = false
		if err != nil {
			break // io.EOF (or a read error): keep what parsed
		}
	}
	return snap, true
}

// scanTranscriptLine folds one transcript line into the snapshot (see
// readTeammateTranscript for the projection).
func scanTranscriptLine(ln []byte, snap *teammateSnapshot) {
	switch {
	case bytes.Contains(ln, []byte(`"assistant"`)):
		var tl transcriptLine
		if json.Unmarshal(ln, &tl) != nil || tl.Type != "assistant" {
			return
		}
		var blocks []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		}
		if json.Unmarshal(tl.Message.Content, &blocks) == nil {
			var texts []string
			for _, c := range blocks {
				switch c.Type {
				case "tool_use":
					if c.Name == "" {
						continue
					}
					sig := c.Name
					if arg := subagent.ToolArgPreview(c.Input); arg != "" {
						sig += "(" + arg + ")"
					}
					snap.activity.sigs = append(snap.activity.sigs, sig)
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						texts = append(texts, c.Text)
					}
				}
			}
			if len(texts) > 0 {
				snap.outputs = append(snap.outputs, mateOutput{text: strings.Join(texts, "\n"), ts: tl.Timestamp})
			}
		}
		if u := tl.Message.Usage; u != nil {
			if ctx := u.InputTokens + u.CacheReadInput; ctx > snap.ctxTok {
				snap.ctxTok = ctx
			}
			snap.outTok += u.OutputTokens
		}
	case bytes.Contains(ln, []byte("<teammate-message")):
		var tl transcriptLine
		if json.Unmarshal(ln, &tl) != nil || tl.Type != "user" {
			return
		}
		var body string
		if json.Unmarshal(tl.Message.Content, &body) != nil {
			return
		}
		if msg, ok := parseTeammateMessage(body, tl.Timestamp); ok {
			snap.msgs = append(snap.msgs, msg)
		}
	}
}

// parseTeammateMessage decodes a consumed message's transcript wrapper:
// `<teammate-message teammate_id="X"[ summary="Y"] …>BODY</teammate-message>`.
// A wrapper without a summary attr previews its body instead.
func parseTeammateMessage(s, ts string) (mateMessage, bool) {
	if !strings.HasPrefix(s, "<teammate-message") {
		return mateMessage{}, false
	}
	header, rest, ok := strings.Cut(s, ">")
	if !ok {
		return mateMessage{}, false
	}
	body := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "</teammate-message>"))
	msg := mateMessage{
		from:    tagAttr(header, "teammate_id"),
		summary: tagAttr(header, "summary"),
		body:    body,
		ts:      ts,
	}
	if msg.summary == "" {
		msg.summary = inboxPreview(body)
	}
	return msg, true
}

// tagAttr extracts a double-quoted attribute value from an opening-tag header.
func tagAttr(header, key string) string {
	_, rest, ok := strings.Cut(header, key+`="`)
	if !ok {
		return ""
	}
	val, _, ok := strings.Cut(rest, `"`)
	if !ok {
		return ""
	}
	return val
}
