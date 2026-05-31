//go:build darwin

package spawn

import (
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// TestInheritPermissionFlags_Darwin_ReadsLeadViaPs: on darwin (no /proc) a
// SUCCESSFUL lead detection must read the lead's argv via internal/procintrospect
// (`ps`) and yield a non-frozen-template source — proving inherit.go does not
// read /proc directly (which always fails on macOS, silently dropping the lead's
// permission intent).
//
// It leaves readLeadCmdline at the PRODUCTION procintrospect-backed reader and
// only stubs detect/revalidate to point at a real child process whose argv
// carries `--permission-mode acceptEdits`. The `; true` tail stops the shell
// from exec-optimizing into sleep, which would drop the marker argv.
func TestInheritPermissionFlags_Darwin_ReadsLeadViaPs(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30; true", "marker", "--permission-mode", "acceptEdits")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sh: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid

	origDetect, origRevalidate := detectLeadPID, revalidateLead
	detectLeadPID = func() (int, string) { return pid, "tok" }
	revalidateLead = func(gotPID int, gotStart string) bool { return gotPID == pid && gotStart == "tok" }
	t.Cleanup(func() { detectLeadPID, revalidateLead = origDetect, origRevalidate })

	// ps can lag a freshly-forked child; retry until its argv is visible.
	var (
		flags []string
		src   string
	)
	for i := 0; i < 40; i++ {
		flags, src = inheritPermissionFlags("")
		if src != "frozen-template" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if src == "frozen-template" {
		t.Fatal("darwin lead read fell back to frozen-template — inherit.go still bypasses procintrospect")
	}
	want := []string{"--permission-mode", "acceptEdits"}
	if src != "lead-flag" || !reflect.DeepEqual(flags, want) {
		t.Fatalf("inheritPermissionFlags via ps = (%v, %q), want (%v, lead-flag)", flags, src, want)
	}
}
