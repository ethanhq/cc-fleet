//go:build !windows

package subagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBackgroundJobSurvivesSettingsRewritingWrapper runs a background job whose
// "claude" is a wrapper that copies --settings to another path and execs a
// second claude-looking script with the new path, as cmux's claude wrapper
// does. Once that script runs, the profile path is gone from argv, yet the job
// must read running; the script holds until the test has checked that, then
// the job must read done.
func TestBackgroundJobSurvivesSettingsRewritingWrapper(t *testing.T) {
	if !hasArgvIntrospection {
		t.Skip("liveness reads argv; unavailable on this platform")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	writeMinimalProviders(t, xdg)

	tmp := t.TempDir()
	merged, args, release := filepath.Join(tmp, "merged-settings.json"), filepath.Join(tmp, "inner-args"), filepath.Join(tmp, "release")
	// Records its argv (by rename, so the test never reads half a file), then waits for release.
	inner := writeFakeBin(t, "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+args+".tmp'\nmv '"+args+".tmp' '"+args+"'\n"+
		"while [ ! -e '"+release+"' ]; do sleep 0.1; done\nprintf '%s\\n' '"+smokeSuccessJSON+"'\n")
	// Rotate the args once, swapping the --settings value for the copy.
	wrapper := writeFakeBin(t, `#!/bin/sh
n=$#
while [ "$n" -gt 0 ]; do
  if [ "$1" = --settings ]; then
    cat "$2" > '`+merged+`'
    set -- "$@" --settings '`+merged+`'
    shift 2; n=$((n-2))
  else
    set -- "$@" "$1"
    shift; n=$((n-1))
  fi
done
exec '`+inner+`' "$@"
`)
	binStubResolver(t, wrapper, "")

	res := Run(context.Background(), Request{Provider: "glm", Prompt: "hi", JSON: true, Background: true, LeadSessionID: "lead-wrap-1"})
	if !res.OK || res.JobID == "" || res.PID <= 0 {
		t.Fatalf("background launch failed: %+v", res)
	}
	// The one reaper for the child, started at once so every path below can wait for it.
	var reapErr error
	reaped := make(chan struct{})
	go func() {
		defer close(reaped)
		for {
			if _, reapErr = syscall.Wait4(res.PID, nil, 0, nil); reapErr != syscall.EINTR {
				return
			}
		}
	}()
	t.Cleanup(func() {
		select {
		case <-reaped:
			return
		default:
		}
		_ = syscall.Kill(-res.PID, syscall.SIGKILL) // still waiting for a release that never came
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Error("job process was not reaped after SIGKILL")
		}
	})

	var innerArgs string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if b, err := os.ReadFile(args); err == nil {
			innerArgs = string(b)
			break
		}
	}
	if !strings.Contains(innerArgs, "--settings\n"+merged+"\n") || strings.Count(innerArgs, "--settings\n") != 1 {
		t.Fatalf("inner script is not running with only the rewritten --settings: %q", innerArgs)
	}
	if st := StatusFor(res.JobID); st.Status != "running" {
		t.Fatalf("job must read running while the rewritten process is alive: %+v", st)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The child is reaped before the last status read, so done is read with the pid gone.
	select {
	case <-reaped:
		if reapErr != nil {
			t.Fatalf("reap the job process: %v", reapErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("job process did not exit after release")
	}
	if st := StatusFor(res.JobID); st.Status != "done" || st.Result != "SUBAGENT_SMOKE_OK=42" {
		t.Fatalf("finished job must read done with its result: %+v", st)
	}
}
