package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ethanhq/cc-fleet/internal/selfupdate"
	"github.com/ethanhq/cc-fleet/internal/version"
)

// maybePromptUpdate shows a once-a-day "new version available" prompt just
// before the TUI starts — while the terminal is still in normal mode, so an
// "update now" re-exec is clean and no global pre-run can ever taint keyget or
// any machine-facing command. It is driven purely by the cached check (no
// network on the launch path); a background goroutine refreshes that cache for
// the next launch. Called only from the bare-interactive both-TTY path.
func maybePromptUpdate() {
	if selfupdate.OptedOut() {
		return
	}
	// Refresh the cache for next time in the background — never blocks launch.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		selfupdate.RefreshCache(ctx, time.Now())
	}()

	now := time.Now()
	tag, ok := selfupdate.PromptTag(now)
	if !ok {
		return
	}

	fmt.Printf("\ncc-fleet %s is available — you're on %s.\n", tag, version.Resolve())
	fmt.Println("  [u] update now      [enter] continue  (asked again tomorrow)")
	fmt.Print("> ")
	choice := strings.ToLower(readLineCooked(os.Stdin))

	// Either choice suppresses the prompt for a day.
	_ = selfupdate.Dismiss(tag, now)
	if choice != "u" {
		return // continue into the TUI on the current version
	}

	// Capture the binary path BEFORE the swap: after a self-update os.Executable
	// resolves to the renamed .previous inode, so the re-exec must target the
	// original path (which now holds the new binary).
	bin, exeErr := os.Executable()
	if exeErr == nil {
		if resolved, e := filepath.EvalSymlinks(bin); e == nil {
			bin = resolved
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := selfupdate.Run(ctx, selfupdate.Options{Out: os.Stdout}); err != nil {
		fmt.Fprintln(os.Stderr, "cc-fleet update:", err)
		return // fall through to the TUI on the current binary
	}
	if runtime.GOOS == "windows" {
		// Run only printed the reinstall notice (npm/zip); there is nothing new on
		// disk to relaunch into, and exec is unavailable here.
		return
	}
	if exeErr != nil {
		fmt.Println("updated — restart ccf to use the new version.")
		return
	}
	fmt.Println("relaunching…")
	if err := syscall.Exec(bin, os.Args, os.Environ()); err != nil {
		// Exec failed — the new binary is on disk; just continue this session.
		fmt.Fprintln(os.Stderr, "cc-fleet: relaunch failed, continuing on the current version:", err)
	}
}

// readLineCooked reads one line from f one byte at a time so it never buffers
// past the newline — the bytes after it belong to bubbletea, which takes over
// stdin once the TUI starts.
func readLineCooked(f *os.File) string {
	var b []byte
	buf := make([]byte, 1)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			b = append(b, buf[0])
		}
		if err != nil {
			break
		}
	}
	return strings.TrimSpace(string(b))
}
