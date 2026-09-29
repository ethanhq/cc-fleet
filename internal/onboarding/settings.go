package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// SettingsEdit is one key edit applied by EditSettings. Path names the key from
// the top level, e.g. {"env","CLAUDE_CODE_TEAMMATE_COMMAND"} or {"teammateMode"}.
type SettingsEdit struct {
	Path   []string // e.g. {"env","CLAUDE_CODE_TEAMMATE_COMMAND"}, {"teammateMode"}
	Value  string   // the string value written when Delete is false
	Delete bool
}

// ErrSettingsShape is returned when the settings document (or an object it is
// edited through, such as env) is not a JSON object.
var ErrSettingsShape = errors.New("settings.json: top level or env is not a JSON object")

// maxSymlinkHops bounds the manual resolution of a dangling symlink chain.
const maxSymlinkHops = 40

// settingsEntry is one key of a JSON object, with its value kept verbatim.
type settingsEntry struct {
	key string
	val json.RawMessage
}

// EditSettings applies edits to the settings file at path and returns the key
// paths (joined with ".") whose value actually changed. It:
//
//   - writes the symlink TARGET (the symlink itself is never replaced); a
//     missing file is treated as {} and created at 0600;
//   - keeps the order of every key, and every untouched value byte-for-byte up
//     to whitespace; a new key is appended to the end of its object, a Delete
//     removes the key;
//   - writes nothing when no value changed;
//   - otherwise re-encodes with a fixed 2-space indent, a trailing newline and
//     no HTML escaping, and replaces the file atomically at its original mode.
//
// A non-object top level, or a non-object value an edit descends through (env),
// returns ErrSettingsShape and writes nothing.
func EditSettings(path string, edits []SettingsEdit) (changed []string, err error) {
	target, err := settingsTarget(path)
	if err != nil {
		return nil, err
	}

	mode := os.FileMode(0o600)
	exists := true
	data, err := os.ReadFile(target)
	switch {
	case err == nil:
		fi, statErr := os.Stat(target)
		if statErr != nil {
			return nil, statErr
		}
		mode = fi.Mode().Perm()
	case errors.Is(err, os.ErrNotExist):
		exists = false
	default:
		return nil, err
	}

	root, err := decodeSettingsObject(data)
	if err != nil {
		return nil, fmt.Errorf("onboarding: %s: %w", target, err)
	}

	for _, e := range edits {
		if len(e.Path) == 0 {
			return nil, errors.New("onboarding: settings edit with empty key path")
		}
		did, err := applySettingsEdit(&root, e.Path, e)
		if err != nil {
			return nil, fmt.Errorf("onboarding: %s: %w", target, err)
		}
		if did {
			key := strings.Join(e.Path, ".")
			if !slices.Contains(changed, key) {
				changed = append(changed, key)
			}
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}

	compact, err := encodeSettingsObject(root)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')

	if !exists {
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return nil, err
		}
	}
	if err := fileutil.AtomicWrite(target, out.Bytes(), mode); err != nil {
		return nil, err
	}
	return changed, nil
}

// SettingsString reads the string value at keyPath in the settings file at
// path. A missing file or key, or a non-string value, returns present=false
// with no error. A non-object top level (or a non-object value on the way to
// the key) returns ErrSettingsShape.
func SettingsString(path string, keyPath ...string) (value string, present bool, err error) {
	if len(keyPath) == 0 {
		return "", false, errors.New("onboarding: SettingsString with empty key path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	obj, err := decodeSettingsObject(data)
	if err != nil {
		return "", false, fmt.Errorf("onboarding: %s: %w", path, err)
	}
	for i, k := range keyPath {
		idx := lastSettingsKey(obj, k)
		if idx < 0 {
			return "", false, nil
		}
		raw := obj[idx].val
		if i == len(keyPath)-1 {
			// Unmarshal accepts null into a string, so check the kind first.
			var s string
			if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '"' || json.Unmarshal(trimmed, &s) != nil {
				return "", false, nil
			}
			return s, true, nil
		}
		if obj, err = decodeSettingsObject(raw); err != nil {
			return "", false, fmt.Errorf("onboarding: %s: %w", path, err)
		}
	}
	return "", false, nil // unreachable: keyPath is non-empty
}

// settingsTarget resolves path to the file that should actually be written:
// the end of its symlink chain, even when that end does not exist yet.
func settingsTarget(path string) (string, error) {
	target, err := filepath.EvalSymlinks(path)
	if err == nil {
		return target, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Missing file, or a dangling symlink: follow the links by hand.
	p := path
	for i := 0; i < maxSymlinkHops; i++ {
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		link, err := os.Readlink(p)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(p), link)
		}
		p = link
	}
	return "", fmt.Errorf("onboarding: %s: too many levels of symbolic links", path)
}

// decodeSettingsObject parses a JSON object into its entries in document order,
// keeping every value as raw bytes. Empty (or whitespace-only) input is {}.
// Anything other than a single JSON object returns ErrSettingsShape, except
// malformed JSON, which returns the decoder's error.
func decodeSettingsObject(data []byte) ([]settingsEntry, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, ErrSettingsShape
	}
	var obj []settingsEntry
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v in object", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		obj = append(obj, settingsEntry{key: key, val: raw})
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing data after JSON object")
		}
		return nil, err
	}
	return obj, nil
}

// encodeSettingsObject renders entries as a compact JSON object without HTML
// escaping; values are emitted verbatim.
func encodeSettingsObject(obj []settingsEntry) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range obj {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalNoEscape(e.key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(e.val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// applySettingsEdit applies e at keyPath inside *obj, descending (and, for a
// set, creating) intermediate objects. It reports whether any value changed.
func applySettingsEdit(obj *[]settingsEntry, keyPath []string, e SettingsEdit) (bool, error) {
	key := keyPath[0]
	idx := lastSettingsKey(*obj, key)

	if len(keyPath) > 1 {
		var child []settingsEntry
		if idx >= 0 {
			var err error
			if child, err = decodeSettingsObject((*obj)[idx].val); err != nil {
				return false, err
			}
		} else if e.Delete {
			return false, nil
		}
		did, err := applySettingsEdit(&child, keyPath[1:], e)
		if err != nil || !did {
			return false, err
		}
		raw, err := encodeSettingsObject(child)
		if err != nil {
			return false, err
		}
		setSettingsEntry(obj, idx, key, raw)
		return true, nil
	}

	if e.Delete {
		kept := (*obj)[:0]
		for _, en := range *obj {
			if en.key != key {
				kept = append(kept, en)
			}
		}
		did := len(kept) != len(*obj)
		*obj = kept
		return did, nil
	}

	if idx >= 0 {
		var cur string
		if json.Unmarshal((*obj)[idx].val, &cur) == nil && cur == e.Value {
			return false, nil
		}
	}
	raw, err := marshalNoEscape(e.Value)
	if err != nil {
		return false, err
	}
	setSettingsEntry(obj, idx, key, raw)
	return true, nil
}

// setSettingsEntry replaces the value at idx in place, or appends key when
// idx < 0.
func setSettingsEntry(obj *[]settingsEntry, idx int, key string, raw json.RawMessage) {
	if idx >= 0 {
		(*obj)[idx].val = raw
		return
	}
	*obj = append(*obj, settingsEntry{key: key, val: raw})
}

// lastSettingsKey returns the index of the last entry named key (the one a JSON
// reader sees when a key is duplicated), or -1.
func lastSettingsKey(obj []settingsEntry, key string) int {
	for i := len(obj) - 1; i >= 0; i-- {
		if obj[i].key == key {
			return i
		}
	}
	return -1
}

// marshalNoEscape encodes v as JSON without HTML escaping and without the
// encoder's trailing newline.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}
