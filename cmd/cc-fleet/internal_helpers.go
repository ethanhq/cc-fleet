package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// promptPassword reads a line from the controlling terminal without echoing it.
// Used wherever an API key is collected interactively, so the key never lands in
// terminal scrollback or shell history. Falls back to a clear error when stdin
// isn't a TTY (defense-in-depth; the caller's non-tty branch should already have
// rejected interactive input).
func promptPassword(prompt string) (string, error) {
	if prompt != "" {
		fmt.Print(prompt)
	}
	fd := os.Stdin.Fd()
	if !term.IsTerminal(fd) {
		fmt.Println()
		return "", errors.New("cannot read password: stdin is not a terminal (use --api-key-stdin)")
	}
	b, err := term.ReadPassword(fd)
	// Always print a newline after the read so the next prompt isn't on the
	// same line as the (invisible) input.
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// readAllFromFd drains an *os.File completely. Wrapped so the add/edit
// commands' --api-key-stdin path has a single, testable seam.
func readAllFromFd(f *os.File) ([]byte, error) {
	return io.ReadAll(f)
}

// emitJSON marshals v as a single-line JSON envelope on stdout. Used by every
// user-layer subcommand's --json path so the skill always sees exactly one
// line per command. Exits 1 on marshal failure (which should never happen
// for the fixed shapes we use).
func emitJSON(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cc-fleet: marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

// userOpFailureEnvelope is the on-failure JSON shape every user-layer
// command emits. The exact fields a particular command would have on success
// are omitted (`omitempty`) so JSON consumers can dispatch on `ok=false` +
// `error_code` first and read details later.
type userOpFailureEnvelope struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	ErrorCode string `json:"error_code"`
}

// reportUserOpErr prints either a JSON failure envelope (with the typed
// error_code if the underlying error is a *userops.Op) or a stderr line and
// exits 1. Returning is impossible; the helper is the last call in any
// failing branch.
func reportUserOpErr(asJSON bool, err error) {
	code := "UNKNOWN"
	var op *userops.Op
	if errors.As(err, &op) {
		code = op.Code
	}
	if asJSON {
		emitJSON(userOpFailureEnvelope{
			OK:        false,
			Error:     err.Error(),
			ErrorCode: code,
		})
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "cc-fleet: %s: %s\n", code, err)
	os.Exit(1)
}

// promptLine reads a single line of input from r, trims trailing CR/LF, and
// returns the resulting string. EOF is mapped to empty string so non-tty
// callers (test fixtures, heredocs) get a deterministic "no answer".
func promptLine(r *bufio.Reader, prompt string) (string, error) {
	if prompt != "" {
		fmt.Print(prompt)
	}
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// isTTY reports whether the given file is connected to a terminal. We need
// this to decide whether `cc-fleet add` may interactively prompt for missing
// flags or must fail fast — non-tty callers (CI, the skill) get the fail-fast
// path so they never block on input.
func isTTY(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	// Char-device + tty bit set means a terminal-like file. On Linux this is
	// equivalent to isatty(); we avoid the syscall package's terminal helpers
	// to keep the dependency surface small.
	return (info.Mode() & os.ModeCharDevice) != 0
}
