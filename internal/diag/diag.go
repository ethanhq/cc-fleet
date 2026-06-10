// Package diag is the --verbose diagnostic sink: a tiny injected logger that
// timestamps, redacts, and serializes step-trace lines. It is dependency-injected
// (never a package global) so one process can hold different sinks — the CLI
// writes stderr, the TUI a 0600 log file. A nil *Logger is a no-op, so callers
// thread it unguarded; a nil logger and a discard-backed one are behaviorally
// identical except for the writes.
package diag

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ethanhq/cc-fleet/internal/redact"
)

// Logger writes one redacted, timestamped line per Logf call. The mutex
// serializes concurrent Logf calls so their lines never interleave — the
// spawn / subagent / teardown orchestrators thread one Logger across
// concurrent leaves. The Logger never closes its writer; whoever opened the
// underlying file owns its lifecycle.
type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

// New returns a Logger over w, or nil for a nil writer.
func New(w io.Writer) *Logger {
	if w == nil {
		return nil
	}
	return &Logger{w: w}
}

// Logf formats one diagnostic line, masks key-like material, prefixes a
// wall-clock timestamp, and writes it under the lock. Write errors are
// dropped — diagnostics never fail the operation they trace. Callers start
// format with a short component tag ("spawn: …") so interleaved traces stay
// legible.
func (l *Logger) Logf(format string, args ...any) {
	if l == nil {
		return
	}
	line := redact.MaskKeyLikeString(fmt.Sprintf(format, args...))
	stamp := time.Now().Format("15:04:05.000")
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.w, "%s %s\n", stamp, line)
}
