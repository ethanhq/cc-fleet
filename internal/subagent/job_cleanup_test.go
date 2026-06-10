//go:build !windows

package subagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// TestLaunchBackground_CleanupOnWriteMetaFailure: after cmd.Start succeeds, a
// forced writeMeta failure MUST:
//   - kill the process group (no leaked vendor child),
//   - remove the .out / .err files (no orphan job artifacts),
//   - surface the failure to the caller (OK=false, ErrCodeFailed).
//
// Otherwise the detached child runs on with no .json meta record — it could
// never be polled or GC'd.
func TestLaunchBackground_CleanupOnWriteMetaFailure(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	// A claude that "hangs" — sleep is long enough that without the cleanup
	// we would still see the process alive when the test ends. We don't
	// actually wait that long; the cleanup must SIGTERM/SIGKILL it.
	fakeClaude := writeFakeBin(t, "#!/bin/sh\nsleep 60\n")
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	// Force writeMeta to fail. Capture the pid that launchBackground started
	// (via the killProcessGroup seam) so we can prove it was killed.
	origWrite := writeMetaFn
	writeMetaFn = func(_ string, _ jobMeta) error {
		return errors.New("forced: disk full")
	}
	t.Cleanup(func() { writeMetaFn = origWrite })

	var killedPID atomic.Int64
	origKill := killProcessGroup
	killProcessGroup = func(pid int) {
		killedPID.Store(int64(pid))
		// Use the real syscall so the child is actually reaped; the seam is
		// just an observation point.
		origKill(pid)
	}
	t.Cleanup(func() { killProcessGroup = origKill })

	res := Run(context.Background(), Request{Vendor: "glm", Prompt: "hi", JSON: true, Background: true})
	if res.OK {
		t.Fatalf("Run(background) should fail when writeMeta fails; got OK=true")
	}
	if res.ErrorCode != ErrCodeFailed {
		t.Errorf("ErrorCode = %q, want SUBAGENT_FAILED", res.ErrorCode)
	}
	pid := int(killedPID.Load())
	if pid <= 0 {
		t.Fatalf("killProcessGroup was never called — leak!")
	}
	// The child should be dead within a short window.
	deadline := time.Now().Add(2 * time.Second)
	alive := true
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			alive = false
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive {
		// Best-effort: don't fail the test if the kernel hasn't reaped yet on
		// a slow CI box, but record the issue.
		t.Logf("warning: pid %d still alive after cleanup window", pid)
	}
	// Reap to avoid leaving a zombie around. Wait briefly so a slow kernel
	// doesn't fail the test if the child is still exiting.
	_, _ = syscall.Wait4(pid, nil, syscall.WNOHANG, nil)

	// Orphan .out / .err MUST be gone.
	dir := filepath.Join(xdg, "cc-fleet", jobsDirName)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".out" || filepath.Ext(e.Name()) == ".err" {
			t.Errorf("orphan job file left behind: %s", e.Name())
		}
	}
}

// TestLaunchBackground_ForcesInnerJSON: even when the caller asks for text-mode
// output (req.OutputFormat = "text"), background launches MUST run claude with
// --output-format json so StatusFor can classify via the envelope rather than a
// placeholder exit code. We detect this by snooping the meta's OutputFormat
// (which stays "text" — the outer print format), then asserting the actual argv
// contained "--output-format json".
func TestLaunchBackground_ForcesInnerJSON(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	// fake claude that records its argv to a file then immediately writes a
	// minimal success envelope to stdout and exits.
	//
	// The argv is written to a sibling .tmp then atomically renamed into place:
	// the script appends one line per arg, so a reader that polls on "file is
	// non-empty" could otherwise observe a PARTIAL argv (e.g. just the leading
	// --dangerously-skip-permissions before --output-format json is appended)
	// and spuriously fail. With the rename, argsLog is either absent or the
	// complete argv — never half-written. (Flake seen under cold full-suite
	// -race load; isolated runs always won the race.)
	argsLog := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("CCF_ARGS_LOG", argsLog)
	script := `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$CCF_ARGS_LOG.tmp"; done
mv "$CCF_ARGS_LOG.tmp" "$CCF_ARGS_LOG"
printf '%s' '` + smokeSuccessJSON + `'
exit 0
`
	fakeClaude := writeFakeBin(t, script)
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	// Caller deliberately requests text output — we MUST still force JSON
	// internally.
	res := Run(context.Background(), Request{
		Vendor:       "glm",
		Prompt:       "hi",
		OutputFormat: "text",
		Background:   true,
	})
	if !res.OK {
		t.Fatalf("Run failed: %s %s", res.ErrorCode, res.ErrorMsg)
	}

	// Wait for the child to publish its (atomically-renamed) argv. The file
	// only appears once fully written, so a non-empty read means the complete
	// argv. 5s tolerates cold-start scheduling under a loaded -race suite.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(argsLog)
		if len(data) > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	// The flag and its value must be ADJACENT (the script writes one arg per
	// line): two loose tokens anywhere in the argv would not prove
	// `--output-format json` was actually emitted as a coupled pair.
	argv := strings.Split(strings.TrimSpace(string(data)), "\n")
	if !argvHasAdjacent(argv, "--output-format", "json") {
		t.Errorf("background argv missing adjacent --output-format json:\n%s", string(data))
	}

	// Reap detached child.
	_, _ = syscall.Wait4(res.PID, nil, syscall.WNOHANG, nil)
}

// TestLaunchBackground_OuterTextStatusEnvelopeAware: with outer text-mode but
// Background=true, the inner argv is forced to --output-format json AND StatusFor
// MUST classify stdout as an envelope. meta.JSON is forced true for background
// launches so the `innerJSON := meta.JSON || meta.OutputFormat == "json"`
// decision doesn't fall to the text-mode classifier, which would bless a JSON
// error envelope as status:"done". This pokes that path end-to-end (fake-claude
// exits with an error envelope, then we poll StatusFor and assert status:"failed").
func TestLaunchBackground_OuterTextStatusEnvelopeAware(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalVendors(t, xdg)

	// An error envelope with HTTP 429 → classify must map this to
	// ErrCodeRateLimited under the envelope-aware branch. In the text-mode
	// fallback, this same stdout would be treated as a successful plain-text
	// result and reported as status:"done".
	const errorEnvelope = `{"type":"result","subtype":"error","is_error":true,"api_error_status":429,
 "result":"rate limit exceeded","num_turns":1,"total_cost_usd":0,
 "modelUsage":{},"permission_denials":[]}`

	script := `#!/bin/sh
printf '%s' '` + errorEnvelope + `'
exit 1
`
	fakeClaude := writeFakeBin(t, script)
	origFP := loadFP
	loadFP = func() (*fingerprint.Fingerprint, error) {
		return &fingerprint.Fingerprint{BinaryPath: fakeClaude}, nil
	}
	t.Cleanup(func() { loadFP = origFP })

	// Caller deliberately requests text output but background=true. StatusFor
	// must classify the envelope and return failed, NOT treat the JSON-on-stdout
	// as a text-mode success.
	res := Run(context.Background(), Request{
		Vendor:       "glm",
		Prompt:       "hi",
		OutputFormat: "text",
		JSON:         false,
		Background:   true,
	})
	if !res.OK {
		t.Fatalf("Run(background) launch failed: %s %s", res.ErrorCode, res.ErrorMsg)
	}

	// Reap the detached child so processAlive() returns false promptly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := syscall.Wait4(res.PID, nil, syscall.WNOHANG, nil); err == nil {
			// Wait4 returns 0,nil while child is still running; only re-poll.
		}
		if err := syscall.Kill(res.PID, 0); err != nil {
			break // ESRCH → child has exited
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Poll StatusFor — the load-bearing assertion: it must return status=failed
	// with a code derived from the envelope (rate-limit), NOT a placeholder
	// status=done from the text-mode fallback.
	var st Result
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st = StatusFor(res.JobID)
		if st.Status != "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if st.Status != "failed" {
		t.Fatalf("StatusFor.Status = %q, want \"failed\" (envelope-aware classification missed); res=%+v", st.Status, st)
	}
	if st.OK {
		t.Fatalf("StatusFor.OK = true on an error envelope; envelope-aware classification missed")
	}
	if st.ErrorCode != ErrCodeRateLimited {
		t.Errorf("StatusFor.ErrorCode = %q, want %q (envelope api_error_status=429 → RATE_LIMITED)", st.ErrorCode, ErrCodeRateLimited)
	}
}
