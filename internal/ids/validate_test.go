package ids

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateTeamName_RejectsPathTraversal: every path-traversal form must fail
// validation BEFORE any filesystem operation runs.
func TestValidateTeamName_RejectsPathTraversal(t *testing.T) {
	cases := []struct {
		in   string
		name string
	}{
		{"", "empty"},
		{".", "dot"},
		{"..", "dotdot"},
		{"../..", "parent-parent"},
		{"../../etc", "deep-parent"},
		{"a/b", "fwd-slash"},
		{`a\b`, "back-slash"},
		{"/abs", "absolute"},
		{"./x", "leading-dot-slash"},
		{"x/..", "trailing-dotdot"},
		{"x/./y", "embedded-dot"},
		{"a\x00b", "nul-byte"},
		{strings.Repeat("x", maxIDLen+1), "too-long"},
		// a leading '%' collides with tmux pane id syntax — a team named "%prod"
		// would be untargetable by teardown/hide/show.
		{"%prod", "leading-percent"},
		{"%", "bare-percent"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateTeamName(tc.in); err == nil {
				t.Fatalf("ValidateTeamName(%q): want error, got nil", tc.in)
			} else if !errors.Is(err, ErrInvalidTeamName) {
				t.Fatalf("ValidateTeamName(%q): err=%v, want ErrInvalidTeamName", tc.in, err)
			}
		})
	}
}

// TestValidateTeamName_AcceptsCommonNames keeps the rule set practical: every
// shape cc-fleet's real users / tests rely on must keep working.
func TestValidateTeamName_AcceptsCommonNames(t *testing.T) {
	cases := []string{
		"myteam",
		"my-team",
		"my_team",
		"team1",
		"Team_123",
		"alpha",
		"_ccf-e2e",
		"a", // single char
	}
	for _, name := range cases {
		if err := ValidateTeamName(name); err != nil {
			t.Errorf("ValidateTeamName(%q): unexpected error %v", name, err)
		}
	}
}

// TestValidateID_LeadingPercent: a leading '%' must be rejected for team AND
// member names (it collides with tmux pane id syntax, which teardown/hide/show
// branch on before validating a name). A '%' elsewhere is fine.
func TestValidateID_LeadingPercent(t *testing.T) {
	const leading = "%x"
	if _, err := NewTeamID(leading); err == nil {
		t.Fatalf("NewTeamID(%q): want error, got nil", leading)
	}
	if _, err := NewAgentName(leading); err == nil {
		t.Fatalf("NewAgentName(%q): want error, got nil", leading)
	}
	// A non-leading percent is not a pane-id collision and stays valid.
	const midPercent = "pro%d"
	if _, err := NewTeamID(midPercent); err != nil {
		t.Fatalf("NewTeamID(%q): unexpected error %v", midPercent, err)
	}
}

// TestValidateMemberName_RejectsPathTraversal: same rules as team, but assert
// the sentinel wraps ErrInvalidMemberName so dispatchers can distinguish.
func TestValidateMemberName_RejectsPathTraversal(t *testing.T) {
	for _, in := range []string{"", "..", "a/b", `c\d`, "/abs"} {
		err := ValidateMemberName(in)
		if err == nil {
			t.Fatalf("ValidateMemberName(%q): want error, got nil", in)
		}
		if !errors.Is(err, ErrInvalidMemberName) {
			t.Fatalf("ValidateMemberName(%q): err=%v, want ErrInvalidMemberName", in, err)
		}
	}
}

// TestEnsureUnderRoot_BlocksEscape: after building a candidate path, the
// under-root check must catch anything that escapes (including absolute paths
// that bypassed the name-level validators, e.g. via a symlink).
func TestEnsureUnderRoot_BlocksEscape(t *testing.T) {
	root := filepath.Join("/tmp", "teams-root")
	good := []string{
		filepath.Join(root, "alpha"),
		filepath.Join(root, "team1", "config.json"),
	}
	for _, p := range good {
		if err := EnsureUnderRoot(root, p); err != nil {
			t.Errorf("EnsureUnderRoot(%q,%q): unexpected error %v", root, p, err)
		}
	}
	bad := []string{
		"/etc/passwd",
		filepath.Join(root, "..", "elsewhere"),
		"/tmp",
	}
	for _, p := range bad {
		if err := EnsureUnderRoot(root, p); err == nil {
			t.Errorf("EnsureUnderRoot(%q,%q): want error, got nil", root, p)
		}
	}
}

// TestEnsureUnderRoot_AcceptsRootItself: the canonical root path (without a
// child component) should be tolerated — callers sometimes ask "is this still
// the root?" before doing further work.
func TestEnsureUnderRoot_AcceptsRootItself(t *testing.T) {
	root := filepath.Join("/tmp", "teams-root")
	if err := EnsureUnderRoot(root, root); err != nil {
		t.Errorf("EnsureUnderRoot(root, root): unexpected error %v", err)
	}
}

// TestValidateID_RejectsWhitespace: a team/member name with whitespace must be
// rejected. macOS recovers argv via space-joined `ps -o command=`, so a
// whitespace-bearing name would split into tokens and the --agent-id reap /
// teardown / ps match would miss it, leaking the provider process.
func TestValidateID_RejectsWhitespace(t *testing.T) {
	for _, s := range []string{"my team", "a\tb", "trailing ", " leading", "a\nb", "a\rb"} {
		if err := ValidateTeamName(s); err == nil {
			t.Errorf("ValidateTeamName(%q) = nil, want error (whitespace must be rejected)", s)
		}
		if err := ValidateMemberName(s); err == nil {
			t.Errorf("ValidateMemberName(%q) = nil, want error (whitespace must be rejected)", s)
		}
	}
	// A plain identifier with no whitespace must still pass.
	if err := ValidateTeamName("worker-1"); err != nil {
		t.Errorf("ValidateTeamName(%q) = %v, want nil", "worker-1", err)
	}
}

// TestValidateJobID_RejectsPathTraversal: a subagent job id flows into the
// jobs-dir path via filepath.Join, so every traversal form must fail (wrapping
// ErrInvalidJobID so the subagent-status entry point can dispatch on it).
func TestValidateJobID_RejectsPathTraversal(t *testing.T) {
	for _, in := range []string{"", ".", "..", "../..", "../../etc/passwd", "a/b", `c\d`, "/abs", "./x", "x/..", "a\x00b", strings.Repeat("x", maxIDLen+1)} {
		err := ValidateJobID(in)
		if err == nil {
			t.Fatalf("ValidateJobID(%q): want error, got nil", in)
		}
		if !errors.Is(err, ErrInvalidJobID) {
			t.Fatalf("ValidateJobID(%q): err=%v, want ErrInvalidJobID", in, err)
		}
	}
}

// TestValidateJobID_AcceptsUUID: the only job ids cc-fleet ever generates are
// uuid.NewString() values, which must keep passing.
func TestValidateJobID_AcceptsUUID(t *testing.T) {
	for _, in := range []string{"550e8400-e29b-41d4-a716-446655440000", "00000000-0000-0000-0000-000000000000"} {
		if err := ValidateJobID(in); err != nil {
			t.Errorf("ValidateJobID(%q): unexpected error %v", in, err)
		}
	}
}
