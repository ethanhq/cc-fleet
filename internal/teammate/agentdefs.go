package teammate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ethanhq/cc-fleet/internal/claudepaths"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// agentDefMode is the mode of a generated agent definition.
const agentDefMode os.FileMode = 0o644

// RenderAgentDef renders the managed agent definition for t. The model:
// line is the sentinel — the type name itself — so a
// teammate that bypasses the launcher asks the provider-less lead API for a
// model that does not exist. The marker lives in the body as an HTML comment,
// keeping the frontmatter free of unknown keys. model's trailing [1m] is
// dropped from the description.
func RenderAgentDef(t AgentType, provider, model string) []byte {
	typ := t.String()
	desc := fmt.Sprintf("cc-fleet provider teammate (%s, %s). Named teammate only: Agent({name, subagent_type: \"%s\", prompt}); no model/isolation/cwd.",
		provider, config.Strip1M(model), typ)
	var b bytes.Buffer
	b.WriteString("---\n")
	b.WriteString("name: " + typ + "\n")
	b.WriteString("description: " + yamlDoubleQuote(desc) + "\n")
	b.WriteString("model: " + typ + "\n")
	b.WriteString("---\n")
	b.WriteString(AgentDefMarker + "\n")
	b.WriteString(`cc-fleet provider teammate placeholder. If you are reading this, the teammate is NOT running on its provider: stop and report "cc-fleet launcher bypassed" to team-lead.` + "\n")
	return b.Bytes()
}

// agentDefTarget is one definition SyncAgentDefs wants on disk.
type agentDefTarget struct {
	file string // ccf-<p>[.slot].md
	data []byte
}

// agentDefTargets lists the definitions for cfg: one per enabled provider whose
// name is not the reserved native name, plus a strong/fast slot definition only
// when that slot is set and differs from the default model (ignoring [1m]).
func agentDefTargets(cfg *config.Config) []agentDefTarget {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []agentDefTarget
	for _, name := range names {
		p := cfg.Providers[name]
		if p == nil || !p.Enabled || name == config.ReservedNativeProvider {
			continue
		}
		add := func(slot Slot, model string) {
			t := AgentType{Provider: name, Slot: slot}
			out = append(out, agentDefTarget{file: t.String() + ".md", data: RenderAgentDef(t, name, model)})
		}
		add(SlotDefault, p.DefaultModel)
		base := config.Strip1M(p.DefaultModel)
		if p.StrongModel != "" && config.Strip1M(p.StrongModel) != base {
			add(SlotStrong, p.StrongModel)
		}
		if p.FastModel != "" && config.Strip1M(p.FastModel) != base {
			add(SlotFast, p.FastModel)
		}
	}
	return out
}

// SyncAgentDefs makes claudepaths.Agents() hold exactly the managed definitions
// cfg calls for: missing or outdated managed files are written, identical ones
// left alone, managed ccf-*.md files no longer wanted are removed. A file with
// one of our names but without AgentDefMarker is never touched; it is reported
// in Conflicts. The first I/O error stops the sync and is returned with the
// partial result.
func SyncAgentDefs(cfg *config.Config) (SyncResult, error) {
	res := newSyncResult()
	if cfg == nil {
		return res, errors.New("teammate: sync agent definitions: nil config")
	}
	dir := claudepaths.Agents()
	if dir == "" {
		return res, errors.New("teammate: cannot resolve the Claude Code agents directory")
	}
	targets := agentDefTargets(cfg)
	want := make(map[string]bool, len(targets))
	for _, tg := range targets {
		want[tg.file] = true
		path := filepath.Join(dir, tg.file)
		fi, err := os.Lstat(path)
		switch {
		case err == nil && !fi.Mode().IsRegular():
			res.Conflicts = append(res.Conflicts, tg.file)
			continue
		case err == nil:
			have, rerr := os.ReadFile(path)
			if rerr != nil {
				return res, fmt.Errorf("teammate: read %s: %w", path, rerr)
			}
			if bytes.Equal(have, tg.data) {
				res.Unchanged = append(res.Unchanged, tg.file)
				continue
			}
			if !hasAgentDefMarker(have) {
				res.Conflicts = append(res.Conflicts, tg.file)
				continue
			}
		case !errors.Is(err, fs.ErrNotExist):
			return res, fmt.Errorf("teammate: stat %s: %w", path, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, fmt.Errorf("teammate: create agents dir: %w", err)
		}
		if err := fileutil.AtomicWrite(path, tg.data, agentDefMode); err != nil {
			return res, fmt.Errorf("teammate: write %s: %w", tg.file, err)
		}
		res.Written = append(res.Written, tg.file)
	}
	err := removeManagedDefs(dir, func(file string) bool { return !want[file] }, &res)
	return res, err
}

// RemoveAgentDefs removes only the ccf-*.md files that carry AgentDefMarker.
// A missing agents directory is not an error.
func RemoveAgentDefs() (SyncResult, error) {
	res := newSyncResult()
	dir := claudepaths.Agents()
	if dir == "" {
		return res, errors.New("teammate: cannot resolve the Claude Code agents directory")
	}
	err := removeManagedDefs(dir, func(string) bool { return true }, &res)
	return res, err
}

// IsManagedDef reports whether path is a regular file carrying AgentDefMarker.
func IsManagedDef(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && hasAgentDefMarker(data)
}

// FindShadowingDefs returns the project-level definitions a lead started in
// leadCwd would load instead of our user-level one: every .claude/agents/**/*.md
// whose frontmatter name equals agentType (by name, not file name), from leadCwd
// up to and including the git root (a .git directory or file). Without a git
// root only leadCwd itself is scanned. The user-level agents directory is
// skipped when a level's .claude/agents happens to be it.
func FindShadowingDefs(leadCwd, agentType string) []string {
	if leadCwd == "" || agentType == "" {
		return nil
	}
	start := filepath.Clean(leadCwd)
	levels := []string{start}
	for dir := start; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			levels = levels[:1] // no git root: leadCwd only
			break
		}
		dir = parent
		levels = append(levels, dir)
	}
	userAgents := claudepaths.Agents()
	// Real paths already walked. Claude Code follows directory symlinks, so the
	// scan does too; this set stops loops and never enters the user-level dir.
	seen := map[string]bool{}
	if real, err := filepath.EvalSymlinks(userAgents); userAgents != "" && err == nil {
		seen[real] = true
	}
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		// WalkDir does not follow a symlinked root; resolve it first.
		root, err := filepath.EvalSymlinks(dir)
		if err != nil || seen[root] {
			return
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree: skip, keep scanning
			}
			if d.IsDir() {
				if seen[path] {
					return filepath.SkipDir
				}
				seen[path] = true
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				if fi, err := os.Stat(path); err == nil && fi.IsDir() {
					walk(path)
					return nil
				}
			}
			if strings.HasSuffix(d.Name(), ".md") && frontmatterName(path) == agentType {
				out = append(out, path)
			}
			return nil
		})
	}
	for _, level := range levels {
		agents := filepath.Join(level, ".claude", "agents")
		if userAgents != "" && samePath(agents, userAgents) {
			continue
		}
		walk(agents)
	}
	return out
}

func newSyncResult() SyncResult {
	return SyncResult{Written: []string{}, Removed: []string{}, Unchanged: []string{}}
}

// removeManagedDefs deletes the managed ccf-*.md files in dir for which drop
// returns true, recording them in res.Removed.
func removeManagedDefs(dir string, drop func(file string) bool, res *SyncResult) error {
	matches, err := filepath.Glob(filepath.Join(dir, TypePrefix+"*.md"))
	if err != nil {
		return fmt.Errorf("teammate: list agent definitions: %w", err)
	}
	sort.Strings(matches)
	for _, path := range matches {
		file := filepath.Base(path)
		if !drop(file) || !IsManagedDef(path) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("teammate: remove %s: %w", file, err)
		}
		res.Removed = append(res.Removed, file)
	}
	return nil
}

// hasAgentDefMarker reports whether data has AgentDefMarker as a whole line.
func hasAgentDefMarker(data []byte) bool {
	for _, line := range bytes.Split(data, []byte("\n")) {
		if string(bytes.TrimRight(line, "\r")) == AgentDefMarker {
			return true
		}
	}
	return false
}

// frontmatterName returns the name: value of the file's YAML frontmatter, or ""
// when the file has none. The value is decoded as a YAML scalar.
func frontmatterName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if strings.TrimPrefix(line, "\ufeff") != "---" {
				return ""
			}
			continue
		}
		if line == "---" {
			return ""
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "name" || key != strings.TrimLeft(key, " \t") {
			continue
		}
		return yamlScalar(strings.TrimSpace(val))
	}
	return ""
}

// yamlScalar decodes a single-line YAML scalar the way a YAML parser reads the
// definition: a double-quoted string with its escapes, a single-quoted
// string where a doubled quote is one quote, or a plain value up to a " #"
// comment. An
// unterminated quoted string yields "".
func yamlScalar(v string) string {
	switch {
	case strings.HasPrefix(v, "'"):
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			if v[i] != '\'' {
				b.WriteByte(v[i])
				continue
			}
			if i+1 < len(v) && v[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String()
		}
		return ""
	case strings.HasPrefix(v, `"`):
		return yamlDoubleUnquote(v[1:])
	}
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			v = v[:i]
			break
		}
	}
	if strings.HasPrefix(v, "#") {
		return ""
	}
	return strings.TrimSpace(v)
}

// yamlDoubleUnquote decodes the body of a YAML double-quoted scalar (after the
// opening quote) up to its closing quote; "" when it is unterminated or holds
// an invalid escape.
func yamlDoubleUnquote(s string) string {
	simple := map[byte]string{'0': "\x00", 'a': "\a", 'b': "\b", 't': "\t", '\t': "\t", 'n': "\n",
		'v': "\v", 'f': "\f", 'r': "\r", 'e': "\x1b", ' ': " ", '"': `"`, '/': "/", '\\': `\`,
		'N': "\u0085", '_': " ", 'L': " ", 'P': " "}
	hexLen := map[byte]int{'x': 2, 'u': 4, 'U': 8}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String()
		case '\\':
			if i+1 >= len(s) {
				return ""
			}
			e := s[i+1]
			if r, ok := simple[e]; ok {
				b.WriteString(r)
				i++
				continue
			}
			n, ok := hexLen[e]
			if !ok || i+2+n > len(s) {
				return ""
			}
			r, err := strconv.ParseUint(s[i+2:i+2+n], 16, 32)
			if err != nil || !utf8.ValidRune(rune(r)) {
				return ""
			}
			b.WriteRune(rune(r))
			i += 1 + n
		default:
			b.WriteByte(c)
		}
	}
	return ""
}

// samePath reports whether a and b name the same directory, comparing
// symlink-resolved paths when both resolve.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// yamlDoubleQuote renders s as a YAML double-quoted scalar: backslash and
// double quote are escaped; control characters and the Unicode line/paragraph
// separators are dropped, so the value always stays on one line.
func yamlDoubleQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case unicode.IsControl(r), r == '\u2028', r == '\u2029':
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
