package codexproxy

import "strconv"

// toolNameMap rewrites Anthropic tool names that fall outside the OpenAI
// function-name charset (^[A-Za-z0-9_-]{1,64}$) on the way out and restores the
// original on the way back, so a non-conforming MCP name (a dotted server.tool, or
// one longer than 64 chars) does not 400 a strict endpoint while the model's
// tool_use still carries the name claude expects. Only NON-conforming names are
// rewritten — a conforming name passes through untouched, so a genuinely malformed
// name is the only thing that ever changes (it is never masked silently for a
// well-formed tool). Built once per request and read-only afterwards, so no lock.
type toolNameMap struct {
	out map[string]string // original -> sanitized (only the names that changed)
	in  map[string]string // sanitized -> original
}

// newToolNameMap returns nil when every tool name already conforms (the common
// case), so the sanitize/restore calls are no-ops with zero allocation.
func newToolNameMap(tools []anthropicTool) *toolNameMap {
	m := &toolNameMap{out: map[string]string{}, in: map[string]string{}}
	// Reserve conforming names first (they pass through unchanged): a sanitized
	// non-conforming name must never collide with a real conforming one, or restore
	// would map the model's call to the wrong tool.
	for _, t := range tools {
		if t.Name != "" && conformingToolName(t.Name) {
			m.in[t.Name] = t.Name
		}
	}
	for _, t := range tools {
		if t.Name == "" || conformingToolName(t.Name) {
			continue
		}
		s := uniqueSanitized(m, t.Name)
		m.out[t.Name] = s
		m.in[s] = t.Name
	}
	if len(m.out) == 0 {
		return nil
	}
	return m
}

func (m *toolNameMap) sanitize(name string) string {
	if m == nil {
		return name
	}
	if s, ok := m.out[name]; ok {
		return s
	}
	return name
}

func (m *toolNameMap) restore(name string) string {
	if m == nil {
		return name
	}
	if orig, ok := m.in[name]; ok {
		return orig
	}
	return name
}

func conformingToolName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// uniqueSanitized maps name into the charset, truncates to 64, and appends a numeric
// suffix if that collides with a DIFFERENT original already mapped (so in stays 1:1).
func uniqueSanitized(m *toolNameMap, name string) string {
	base := truncateName(sanitizeChars(name), 64)
	if base == "" {
		base = "tool"
	}
	s := base
	for n := 2; ; n++ {
		if orig, taken := m.in[s]; !taken || orig == name {
			return s
		}
		suf := "_" + strconv.Itoa(n)
		s = truncateName(base, 64-len(suf)) + suf
	}
}

func sanitizeChars(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b = append(b, byte(r))
		} else {
			b = append(b, '_')
		}
	}
	return string(b)
}

func truncateName(s string, n int) string {
	if n < 0 {
		n = 0
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}
