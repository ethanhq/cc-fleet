//go:build !windows

package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunClaude_OutputCapKillsGroup: a child that floods stdout past the cap is
// SIGKILLed (the whole group), runClaude returns errOutputTooLarge promptly (not
// at the generous ctx deadline), and the captured stdout stays bounded — never a
// hang, never unbounded buffering.
func TestRunClaude_OutputCapKillsGroup(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	// Flood >cap to stdout, then hang: only the overflow-kill (not the 10s ctx
	// deadline) can end this run quickly.
	bin := writeFakeBin(t, "#!/bin/sh\nyes | head -c 100000\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	stdout, _, _, err := runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "", false, nil, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("err = %v, want errOutputTooLarge", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runClaude took %v — the overflow kill did not end the flood promptly", elapsed)
	}
	if len(stdout) > maxChildOutput {
		t.Fatalf("captured stdout = %d bytes, want <= cap %d (must not buffer unbounded)", len(stdout), maxChildOutput)
	}
}

// TestRunClaude_OutputCapBothStreams floods stdout AND stderr past the cap
// concurrently, so the race detector exercises both copy goroutines driving the
// shared kill-group / sync.Once path (the stdout-only test never does). Asserts a
// clean OUTPUT_TOO_LARGE outcome with both captures bounded.
func TestRunClaude_OutputCapBothStreams(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	script := "#!/bin/sh\n" +
		"{ yes | head -c 100000; } &\n" +
		"{ yes | head -c 100000 1>&2; } &\n" +
		"sleep 30\n"
	bin := writeFakeBin(t, script)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	stdout, stderr, _, err := runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "", false, nil, nil)
	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("err = %v, want errOutputTooLarge", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("both-stream flood not ended promptly by the overflow kill")
	}
	if len(stdout) > maxChildOutput || len(stderr) > maxChildOutput {
		t.Fatalf("capture exceeded cap: stdout=%d stderr=%d cap=%d", len(stdout), len(stderr), maxChildOutput)
	}
}

// TestRun_OutputTooLarge_Surfaces: end-to-end, a provider child that floods stdout
// past the cap surfaces as SUBAGENT_OUTPUT_TOO_LARGE — not a misclassified
// SUBAGENT_FAILED from a truncated envelope.
func TestRun_OutputTooLarge_Surfaces(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })

	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	// Fake claude: answer --version for ResolveBinaryPath, else flood >cap then hang.
	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in --version) echo \"2.1.150\"; exit 0;; esac\n" +
		"yes | head -c 100000\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := `version = 1

[glm]
base_url        = "https://example.invalid/anthropic"
default_model   = "glm-4.6"
models_endpoint = "https://example.invalid/v1/models"
secret_backend  = "file"
secret_ref      = "glm.key"
enabled         = true
added_at        = 2026-05-24T05:00:00Z
`
	if err := os.WriteFile(filepath.Join(dir, "providers.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}

	res := Run(context.Background(), Request{Provider: "glm", Prompt: "hi", JSON: true, Timeout: 10 * time.Second})
	if res.OK {
		t.Fatal("OK should be false on output overflow")
	}
	if res.ErrorCode != ErrCodeOutputTooLarge {
		t.Fatalf("ErrorCode = %s (msg=%s), want SUBAGENT_OUTPUT_TOO_LARGE", res.ErrorCode, res.ErrorMsg)
	}
}

// streamFlood is a fake stream-json child: a transcript of short lines far past a 4 KiB cap, then
// the result line, then a trailing hook line.
const streamFloodResult = `{"type":"result","subtype":"success","is_error":false,"result":"done"}`

const streamFlood = "#!/bin/sh\n" +
	"cat > /dev/null\n" +
	"i=0; while [ $i -lt 2000 ]; do echo '{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\"}}'; i=$((i+1)); done\n" +
	"echo '" + streamFloodResult + "'\n" +
	"echo '{\"type\":\"system\",\"subtype\":\"hook_response\"}'\n"

// TestRunClaude_StreamJSONKeepsOnlyResult: a stream-json transcript's volume never counts against
// the cap — the child runs to completion and stdout is just its result line — while one line over
// the cap still trips the overflow kill.
func TestRunClaude_StreamJSONKeepsOnlyResult(t *testing.T) {
	origCap, origGrace := maxChildOutput, waitGrace
	maxChildOutput = 4096
	waitGrace = 200 * time.Millisecond
	t.Cleanup(func() { maxChildOutput, waitGrace = origCap, origGrace })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bin := writeFakeBin(t, streamFlood)
	stdout, _, code, err := runClaude(ctx, bin, []string{bin}, os.Environ(), nil, "", true, nil, nil)
	if err != nil || code != 0 {
		t.Fatalf("err = %v, exit %d; want a clean run", err, code)
	}
	if string(stdout) != streamFloodResult+"\n" {
		t.Fatalf("stdout = %q, want only the result line", stdout)
	}
	tail := writeFakeBin(t, "#!/bin/sh\necho '{\"type\":\"assistant\"}'\nprintf '%s' '"+streamFloodResult+"'\n")
	if stdout, _, _, err := runClaude(ctx, tail, []string{tail}, os.Environ(), nil, "", true, nil, nil); err != nil || string(stdout) != streamFloodResult {
		t.Fatalf("result line without a trailing newline: stdout = %q, err = %v", stdout, err)
	}

	long := writeFakeBin(t, "#!/bin/sh\nhead -c 100000 /dev/zero | tr '\\0' a\nsleep 30\n")
	start := time.Now()
	if _, _, _, err := runClaude(ctx, long, []string{long}, os.Environ(), nil, "", true, nil, nil); !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("one over-cap line: err = %v, want errOutputTooLarge", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the over-cap line was not killed promptly")
	}
}

// TestResultLineWriter_SplitWrites: lines split across writes at any byte, and a final result line
// with no trailing newline, still come back as the last result line.
func TestResultLineWriter_SplitWrites(t *testing.T) {
	stream := `{"type":"result","result":"first"}` + "\n" + `{"type":"assistant"}` + "\n" + `{"type":"result","result":"last"}`
	w := &resultLineWriter{limit: 64}
	for i := 0; i < len(stream); i += 3 {
		_, _ = w.Write([]byte(stream[i:min(i+3, len(stream))]))
	}
	w.flush()
	if w.overflow || string(w.result) != `{"type":"result","result":"last"}` {
		t.Fatalf("overflow %v, result %q; want the last result line", w.overflow, w.result)
	}
}

// TestRun_StreamActivityLongTranscript: end-to-end, a stream-json leaf (workflow leaf / --json
// subagent) whose transcript runs past the cap succeeds with its answer.
func TestRun_StreamActivityLongTranscript(t *testing.T) {
	origCap := maxChildOutput
	maxChildOutput = 4096
	t.Cleanup(func() { maxChildOutput = origCap })
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)
	binStubResolver(t, writeFakeBin(t, streamFlood), "")

	res := Run(context.Background(), Request{Provider: "glm", Prompt: "hi", JSON: true, StreamActivity: true, Timeout: 10 * time.Second})
	if !res.OK || res.Result != "done" {
		t.Fatalf("OK=%v result=%q code=%s (%s); want the answer", res.OK, res.Result, res.ErrorCode, res.ErrorMsg)
	}
}
