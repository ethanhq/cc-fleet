package diag

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var lineRE = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}\.\d{3} spawn: step$`)

func TestLogfFormat(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Logf("spawn: %s", "step")
	got := strings.TrimSuffix(buf.String(), "\n")
	if !lineRE.MatchString(got) {
		t.Fatalf("line %q does not match timestamp+message shape", got)
	}
}

func TestLogfMasksKeyLike(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Logf("probe with sk-abcdef1234567890")
	if strings.Contains(buf.String(), "sk-abcdef1234567890") {
		t.Fatalf("key-like material leaked: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "sk-[REDACTED]") {
		t.Fatalf("expected redaction placeholder, got %q", buf.String())
	}
}

func TestNilLoggerNoOp(t *testing.T) {
	var l *Logger
	l.Logf("never written %s", "anywhere") // must not panic
	if New(nil) != nil {
		t.Fatal("New(nil) must return a nil Logger")
	}
}

func TestConcurrentLogfLinesIntact(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Logf("worker line")
			}
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 16*50 {
		t.Fatalf("expected %d lines, got %d", 16*50, len(lines))
	}
	for _, ln := range lines {
		if !strings.HasSuffix(ln, " worker line") {
			t.Fatalf("interleaved/corrupt line: %q", ln)
		}
	}
}
