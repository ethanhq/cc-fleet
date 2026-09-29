package teammate

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// shimMode is the shim's file mode: Claude Code runs it as a command.
const shimMode os.FileMode = 0o755

// shimCcfLine is the only variable line of the shim: ccf='<pinned cc-fleet>'.
const shimCcfLine = "ccf="

// shimTemplateHead and shimTemplateTail are the shim, split around the ccf=
// line. The probe doubles as the shim ↔ binary protocol handshake: a binary
// that does not know __teammate-launch (0.3.x, missing, broken) fails it, so
// native teammates fall back to plain claude while ccf-* teammates are
// refused (fail-closed).
const shimTemplateHead = `#!/bin/sh
# cc-fleet teammate launcher shim — managed-by: cc-fleet (shim protocol 1).
# Claude Code runs this for EVERY split-pane teammate (CLAUDE_CODE_TEAMMATE_COMMAND).
# Regenerate: cc-fleet repair.  Remove: cc-fleet teammate setup --remove.
`

const shimTemplateTail = `if [ -x "$ccf" ] && "$ccf" __teammate-launch --shim-protocol 1 >/dev/null 2>&1; then
  exec "$ccf" __teammate-launch "$@"
fi
# cc-fleet missing / downgraded / broken: native teammates still start; provider teammates are refused.
id= ty= want=
for a in "$@"; do
  case "$want" in
    id) id=$a; want=; continue ;;
    ty) ty=$a; want=; continue ;;
  esac
  case "$a" in
    --agent-id) want=id ;;
    --agent-id=*) id=${a#--agent-id=} ;;
    --agent-type) want=ty ;;
    --agent-type=*) ty=${a#--agent-type=} ;;
  esac
done
case "$ty" in
  ccf-*) echo "cc-fleet teammate: TEAMMATE_SETUP_REQUIRED: $id: launcher_target_missing ($ccf) — run: cc-fleet repair" >&2
         exit 1 ;;
esac
if command -v claude >/dev/null 2>&1; then exec claude "$@"; fi
exec "$HOME/.local/bin/claude" "$@"
`

// ShimPath returns <config.ConfigDir()>/bin/claude-teammate.
func ShimPath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bin", ShimFileName), nil
}

// RenderShim renders the launcher shim with ccfPath pinned on its ccf= line,
// POSIX single-quoted.
func RenderShim(ccfPath string) []byte {
	var b bytes.Buffer
	b.WriteString(shimTemplateHead)
	b.WriteString(shimCcfLine + shQuote(ccfPath) + "\n")
	b.WriteString(shimTemplateTail)
	return b.Bytes()
}

// WriteShim writes the shim at ShimPath() pinned to ccfPath (""
// pins EvalSymlinks(os.Executable())). Identical content at mode 0755 is left
// alone (changed=false); identical content at another mode is only chmod-ed back
// to 0755 (changed=true), so `cc-fleet repair` heals a `chmod a-x`.
func WriteShim(ccfPath string) (path string, changed bool, err error) {
	if ccfPath == "" {
		if ccfPath, err = selfPath(); err != nil {
			return "", false, fmt.Errorf("teammate: resolve cc-fleet binary: %w", err)
		}
	}
	path, err = ShimPath()
	if err != nil {
		return "", false, err
	}
	want := RenderShim(ccfPath)
	if fi, statErr := os.Stat(path); statErr == nil && fi.Mode().IsRegular() {
		if have, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(have, want) {
			if fi.Mode().Perm() == shimMode {
				return path, false, nil
			}
			if err := os.Chmod(path, shimMode); err != nil {
				return path, false, fmt.Errorf("teammate: chmod shim: %w", err)
			}
			return path, true, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, false, fmt.Errorf("teammate: create shim dir: %w", err)
	}
	if err := fileutil.AtomicWrite(path, want, shimMode); err != nil {
		return path, false, fmt.Errorf("teammate: write shim: %w", err)
	}
	return path, true, nil
}

// InspectShim reports the state of the shim at path. It never fails: every
// unreadable part is simply reported as absent.
func InspectShim(path string) ShimStatus {
	st := ShimStatus{Path: path}
	fi, err := os.Stat(path)
	if err != nil {
		return st
	}
	st.Exists = true
	st.Executable = fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
	data, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	st.Managed = bytes.Contains(data, []byte(ShimMarker))
	pinned, ok := parsePinned(data)
	if !ok {
		return st
	}
	st.Pinned = pinned
	pfi, err := os.Stat(pinned)
	if err != nil {
		return st
	}
	st.PinnedExists = true
	if self, err := os.Executable(); err == nil {
		if sfi, err := os.Stat(self); err == nil {
			st.PinnedIsSelf = os.SameFile(pfi, sfi)
		}
	}
	return st
}

// IsOurShim reports whether a settings value names cc-fleet's shim: it equals
// ShimPath(), or the file it names carries ShimMarker.
func IsOurShim(value string) bool {
	if value == "" {
		return false
	}
	if p, err := ShimPath(); err == nil && value == p {
		return true
	}
	fi, err := os.Stat(value)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(value)
	return err == nil && bytes.Contains(data, []byte(ShimMarker))
}

// selfPath is the running binary with symlinks resolved — the path a shim pins.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// shQuote quotes s for POSIX sh with single quotes; an embedded quote closes
// the quoted segment, is emitted as \', and reopens a new segment. Same rule as tmux.Quote, kept separate because the tmux package sheds
// everything but Quote later in this change.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shUnquote reverses shQuote for a value made only of single-quoted segments
// joined by \' escapes (exactly what shQuote emits).
func shUnquote(s string) (string, bool) {
	var b strings.Builder
	for len(s) > 0 {
		switch {
		case s[0] == '\'':
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return "", false
			}
			b.WriteString(s[1 : 1+end])
			s = s[2+end:]
		case strings.HasPrefix(s, `\'`):
			b.WriteByte('\'')
			s = s[2:]
		default:
			return "", false
		}
	}
	return b.String(), true
}

// parsePinned extracts the pinned cc-fleet path from the shim's ccf= line.
func parsePinned(data []byte) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, shimCcfLine) {
			continue
		}
		v, ok := shUnquote(strings.TrimPrefix(line, shimCcfLine))
		if !ok || v == "" {
			return "", false
		}
		return v, true
	}
	return "", false
}
