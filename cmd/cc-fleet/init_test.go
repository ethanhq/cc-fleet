package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

const sentinelKey = "sk-SENTINEL-must-not-echo-123"

// TestInteractiveAdd_KeyNotReadFromEchoingReader: the API key must be read via
// promptPassword (term.ReadPassword, no echo), NOT promptLine over the bufio
// reader (whose contents echo to scrollback on a real TTY).
//
// Proof without a PTY or real key: feed the four non-secret fields through the
// reader, then a sentinel "key" line. promptPassword reads os.Stdin directly and
// refuses on a non-terminal, so interactiveAdd errors at the key step leaving the
// sentinel UNCONSUMED. Had the code used promptLine for the key, the reader would
// be drained. Assert: (a) promptPassword's non-TTY error, (b) sentinel still
// buffered, (c) sentinel never reached stdout.
func TestInteractiveAdd_KeyNotReadFromEchoingReader(t *testing.T) {
	// os.Stdin in `go test` is not a terminal, so promptPassword takes its
	// non-TTY guard branch and returns an error before reading any bytes.
	input := strings.Join([]string{
		"glm",                               // vendor name
		"https://api.example.com/anthropic", // base_url
		"https://api.example.com/v1/models", // models_endpoint
		"glm-4.6",                           // default_model
		sentinelKey,                         // would-be key line (must NOT be consumed)
		"",
	}, "\n")
	reader := bufio.NewReader(strings.NewReader(input))

	// Capture stdout so we can assert the sentinel never echoes.
	out := captureStdout(t)

	err := interactiveAdd(reader)
	stdout := out()

	if err == nil {
		t.Fatal("interactiveAdd: want error from promptPassword on non-TTY stdin, got nil")
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("interactiveAdd err = %q, want promptPassword non-TTY error", err.Error())
	}
	// The sentinel "key" line must still be buffered — proof the key step did
	// NOT read from the echoing bufio reader.
	rest, _ := reader.ReadString('\n')
	if !strings.Contains(rest, sentinelKey) {
		t.Fatalf("sentinel key was consumed from the echoing reader (rest=%q); key must come from promptPassword", rest)
	}
	if strings.Contains(stdout, sentinelKey) {
		t.Fatalf("sentinel key echoed to stdout: %q", stdout)
	}
}

// captureStdout redirects os.Stdout to a pipe for the duration of the returned
// closure's call, returning the captured text. Restores on cleanup.
func captureStdout(t *testing.T) func() string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, e := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if e != nil {
				break
			}
		}
		done <- b.String()
	}()
	return func() string {
		os.Stdout = orig
		_ = w.Close()
		s := <-done
		_ = r.Close()
		return s
	}
}
