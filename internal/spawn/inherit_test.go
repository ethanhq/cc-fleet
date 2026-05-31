package spawn

import (
	"reflect"
	"testing"
)

// setLead points the inheritPermissionFlags seams at a canned lead PID +
// argv for the duration of t. argv == nil with pid != 0 models a lead whose
// cmdline could not be read (readLeadCmdline returns ok=false);
// pid == 0 models "no validated lead" (DetectPIDWithStart returned 0). Both
// restore on cleanup. This keeps the tests hermetic — no real process, no
// leadsession internals — which is why dev's first cut (mocking leadsession's
// procRoot while the lead reader still hit the real /proc) could never pass.
//
// detectLeadPID also yields a procStart token and inherit revalidates it after
// the cmdline read. setLead pins a fixed token and a revalidateLead that AGREES
// (the no-PID-reuse case), so every test exercises the normal inherit path.
// setLeadWithReuse models the detect→read PID-reuse window.
func setLead(t *testing.T, pid int, argv []string) {
	t.Helper()
	setLeadWithReuse(t, pid, "start-token", argv, true)
}

// setLeadWithReuse is setLead plus explicit control over the procStart
// revalidation outcome. revalidates=false models a lead whose /proc start time
// CHANGED between detect and cmdline read (PID reuse) — inherit must fall back
// to frozen-template and inherit NO flags.
func setLeadWithReuse(t *testing.T, pid int, start string, argv []string, revalidates bool) {
	t.Helper()
	origDetect, origRead, origRevalidate := detectLeadPID, readLeadCmdline, revalidateLead
	detectLeadPID = func() (int, string) { return pid, start }
	readLeadCmdline = func(int) ([]string, bool) {
		if argv == nil {
			return nil, false
		}
		return argv, true
	}
	revalidateLead = func(gotPID int, gotStart string) bool {
		return revalidates && gotPID == pid && gotStart == start
	}
	t.Cleanup(func() {
		detectLeadPID, readLeadCmdline, revalidateLead = origDetect, origRead, origRevalidate
	})
}

func TestInheritPermissionFlags_LeadFlag_DangerouslyBypass(t *testing.T) {
	setLead(t, 4242, []string{"claude", "--dangerously-skip-permissions", "--model", "claude-opus-4-7"})

	flags, src := inheritPermissionFlags("")
	want := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(flags, want) || src != "lead-flag" {
		t.Fatalf("inheritPermissionFlags = (%v, %q), want (%v, lead-flag)", flags, src, want)
	}
}

func TestInheritPermissionFlags_LeadFlag_AcceptEdits(t *testing.T) {
	setLead(t, 4243, []string{"claude", "--permission-mode", "acceptEdits"})

	flags, src := inheritPermissionFlags("")
	want := []string{"--permission-mode", "acceptEdits"}
	if !reflect.DeepEqual(flags, want) || src != "lead-flag" {
		t.Fatalf("inheritPermissionFlags = (%v, %q), want (%v, lead-flag)", flags, src, want)
	}
}

func TestInheritPermissionFlags_LeadFlag_Auto(t *testing.T) {
	setLead(t, 4244, []string{"claude", "--permission-mode", "auto"})

	flags, src := inheritPermissionFlags("")
	want := []string{"--permission-mode", "auto"}
	if !reflect.DeepEqual(flags, want) || src != "lead-flag" {
		t.Fatalf("inheritPermissionFlags = (%v, %q), want (%v, lead-flag)", flags, src, want)
	}
}

func TestInheritPermissionFlags_LeadFlag_BypassMode(t *testing.T) {
	// --permission-mode bypassPermissions must collapse to the bare
	// --dangerously-skip-permissions flag.
	setLead(t, 4245, []string{"claude", "--permission-mode", "bypassPermissions"})

	flags, src := inheritPermissionFlags("")
	want := []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(flags, want) || src != "lead-flag" {
		t.Fatalf("inheritPermissionFlags = (%v, %q), want (%v, lead-flag)", flags, src, want)
	}
}

func TestInheritPermissionFlags_LeadDefault_Plan(t *testing.T) {
	setLead(t, 4246, []string{"claude", "--permission-mode", "plan"})

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "lead-default" {
		t.Fatalf("inheritPermissionFlags(plan) = (%v, %q), want (nil, lead-default)", flags, src)
	}
}

func TestInheritPermissionFlags_LeadDefault_Default(t *testing.T) {
	setLead(t, 4247, []string{"claude", "--permission-mode", "default"})

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "lead-default" {
		t.Fatalf("inheritPermissionFlags(default) = (%v, %q), want (nil, lead-default)", flags, src)
	}
}

func TestInheritPermissionFlags_LeadDefault_NoFlag(t *testing.T) {
	setLead(t, 4248, []string{"claude", "--model", "claude-opus-4-7"})

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "lead-default" {
		t.Fatalf("inheritPermissionFlags(no flag) = (%v, %q), want (nil, lead-default)", flags, src)
	}
}

func TestInheritPermissionFlags_FrozenTemplate_NoLead(t *testing.T) {
	// DetectPID() == 0 → frozen-template (covers macOS / out-of-tmux / external
	// shell where the ancestor walk finds no validated Claude session).
	setLead(t, 0, nil)

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "frozen-template" {
		t.Fatalf("inheritPermissionFlags(no lead) = (%v, %q), want (nil, frozen-template)", flags, src)
	}
}

func TestInheritPermissionFlags_FrozenTemplate_CmdlineUnreadable(t *testing.T) {
	// DetectPID() succeeds but readLeadCmdline fails → frozen-template (pure β,
	// no γ split: we do NOT downgrade to default-safe on a read failure).
	setLead(t, 4249, nil)

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "frozen-template" {
		t.Fatalf("inheritPermissionFlags(cmdline unreadable) = (%v, %q), want (nil, frozen-template)", flags, src)
	}
}

func TestInheritPermissionFlags_FrozenTemplate_PIDReuse(t *testing.T) {
	// The lead PID resolves and its cmdline reads (here a process that would
	// yield --dangerously-skip-permissions), but the start time CHANGED between
	// detect and read — the PID was recycled for an unrelated process. inherit
	// MUST fall back to frozen-template and inherit NO flags rather than trust
	// the recycled PID's cmdline.
	setLeadWithReuse(t, 4250, "start-token",
		[]string{"unrelated-proc", "--dangerously-skip-permissions"}, false)

	flags, src := inheritPermissionFlags("")
	if flags != nil || src != "frozen-template" {
		t.Fatalf("inheritPermissionFlags(PID reuse) = (%v, %q), want (nil, frozen-template) — must NOT inherit from recycled PID", flags, src)
	}
}

func TestInheritPermissionFlags_Manual_OverridesLead(t *testing.T) {
	// Even when the lead says bypass, manual override wins and never consults
	// the lead seams.
	setLead(t, 4250, []string{"claude", "--dangerously-skip-permissions"})

	flags, src := inheritPermissionFlags("acceptEdits")
	want := []string{"--permission-mode", "acceptEdits"}
	if !reflect.DeepEqual(flags, want) || src != "manual" {
		t.Fatalf("inheritPermissionFlags(manual acceptEdits) = (%v, %q), want (%v, manual)", flags, src, want)
	}

	flags, src = inheritPermissionFlags("bypassPermissions")
	want = []string{"--dangerously-skip-permissions"}
	if !reflect.DeepEqual(flags, want) || src != "manual" {
		t.Fatalf("inheritPermissionFlags(manual bypass) = (%v, %q), want (%v, manual)", flags, src, want)
	}

	// Manual "default" / "plan" → no flag, but source still manual so the
	// fingerprint template's permission flag gets stripped downstream.
	flags, src = inheritPermissionFlags("default")
	if flags != nil || src != "manual" {
		t.Fatalf("inheritPermissionFlags(manual default) = (%v, %q), want (nil, manual)", flags, src)
	}
	flags, src = inheritPermissionFlags("plan")
	if flags != nil || src != "manual" {
		t.Fatalf("inheritPermissionFlags(manual plan) = (%v, %q), want (nil, manual)", flags, src)
	}
}

func TestStripPermissionFlags(t *testing.T) {
	in := []string{
		"--agent-id", "x@t",
		"--dangerously-skip-permissions",
		"--agent-type", "general-purpose",
		"--permission-mode", "acceptEdits",
		"--agent-name", "x",
	}
	want := []string{
		"--agent-id", "x@t",
		"--agent-type", "general-purpose",
		"--agent-name", "x",
	}
	got := stripPermissionFlags(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stripPermissionFlags = %v, want %v", got, want)
	}

	// No-permission-flag input is returned unchanged in content.
	plain := []string{"--agent-id", "x@t", "--agent-name", "x"}
	got = stripPermissionFlags(plain)
	if !reflect.DeepEqual(got, plain) {
		t.Fatalf("stripPermissionFlags(plain) = %v, want %v", got, plain)
	}
}

func TestFlagValueAndBareFlag(t *testing.T) {
	args := []string{"claude", "--permission-mode", "auto", "--dangerously-skip-permissions"}
	if !hasBareFlag(args, "--dangerously-skip-permissions") {
		t.Fatal("hasBareFlag should find --dangerously-skip-permissions")
	}
	if hasBareFlag(args, "--nope") {
		t.Fatal("hasBareFlag should not find absent flag")
	}
	if v, ok := flagValue(args, "--permission-mode"); !ok || v != "auto" {
		t.Fatalf("flagValue(--permission-mode) = (%q, %v), want (auto, true)", v, ok)
	}
	// Trailing flag with no value following → not ok.
	if v, ok := flagValue([]string{"claude", "--permission-mode"}, "--permission-mode"); ok || v != "" {
		t.Fatalf("flagValue(trailing) = (%q, %v), want (\"\", false)", v, ok)
	}
	// Empty / nil argv is safe.
	if hasBareFlag(nil, "--x") {
		t.Fatal("hasBareFlag(nil) must be false")
	}
	if _, ok := flagValue(nil, "--x"); ok {
		t.Fatal("flagValue(nil) must be !ok")
	}
}
