package onboarding

import (
	"runtime"
	"testing"
)

// TestNeedsTmuxSetup_WindowsNeverPrompts pins the windows short-circuit: the
// tmux teammate lane is unix-only, so the first-run TUI must not park on a tmux
// install screen. On other platforms NeedsTmuxSetup must still honor a prior
// "skip" ack, so a TmuxAck'd state returns false regardless of tmux presence.
func TestNeedsTmuxSetup_WindowsNeverPrompts(t *testing.T) {
	setupHome(t)
	if runtime.GOOS == "windows" {
		if NeedsTmuxSetup() {
			t.Fatal("NeedsTmuxSetup on windows: want false (tmux lane is unix-only)")
		}
		return
	}
	in := State{TmuxAck: true}
	if err := in.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if NeedsTmuxSetup() {
		t.Fatal("NeedsTmuxSetup with TmuxAck set: want false")
	}
}
