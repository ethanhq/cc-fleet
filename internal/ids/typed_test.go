package ids

import (
	"errors"
	"testing"
)

// TestNewTeamID_RejectsInvalid: every input ValidateTeamName rejects must also
// be rejected by NewTeamID. The two MUST share rules, else a caller switching
// between them would change which inputs are accepted.
func TestNewTeamID_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",      // empty
		".",     // dot
		"..",    // double-dot
		"a/b",   // separator
		"a\\b",  // backslash
		"/abs",  // absolute
		"./x",   // clean(./x) = x
		"x/..",  // clean(x/..) = .
		"a\x00", // NUL byte
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			id, err := NewTeamID(in)
			if err == nil {
				t.Fatalf("NewTeamID(%q): want error, got nil (value=%q)", in, id)
			}
			if !errors.Is(err, ErrInvalidTeamName) {
				t.Fatalf("NewTeamID(%q): err=%v, want ErrInvalidTeamName", in, err)
			}
			// Defensive: on failure the typed value must be the zero value so
			// a caller that forgot to check err can't accidentally use a
			// half-validated id.
			if id != "" {
				t.Fatalf("NewTeamID(%q) failure path returned non-empty id %q", in, id)
			}
		})
	}
}

// TestNewTeamID_AcceptsCommonNames keeps the rule set practical: anything the
// validator accepts the typed constructor must also accept.
func TestNewTeamID_AcceptsCommonNames(t *testing.T) {
	cases := []string{"alpha", "my-team", "_test", "team1", "a.b.c"}
	for _, in := range cases {
		id, err := NewTeamID(in)
		if err != nil {
			t.Errorf("NewTeamID(%q): unexpected err %v", in, err)
		}
		if string(id) != in {
			t.Errorf("NewTeamID(%q).String() = %q, want %q", in, id, in)
		}
	}
}

// TestNewAgentName_RejectsInvalid mirrors NewTeamID but checks the member-name
// sentinel — they share rules but must wrap distinct errors so callers can
// dispatch.
func TestNewAgentName_RejectsInvalid(t *testing.T) {
	cases := []string{"", ".", "..", "a/b", "/abs"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			n, err := NewAgentName(in)
			if err == nil {
				t.Fatalf("NewAgentName(%q): want error", in)
			}
			if !errors.Is(err, ErrInvalidMemberName) {
				t.Fatalf("NewAgentName(%q): err=%v, want ErrInvalidMemberName", in, err)
			}
			if n != "" {
				t.Fatalf("NewAgentName(%q) failure returned non-empty %q", in, n)
			}
		})
	}
}

// TestNewAgentName_Accepts confirms common shapes work.
func TestNewAgentName_Accepts(t *testing.T) {
	for _, in := range []string{"alice", "worker-1", "w_2", "a"} {
		n, err := NewAgentName(in)
		if err != nil {
			t.Errorf("NewAgentName(%q): %v", in, err)
		}
		if n.String() != in {
			t.Errorf("NewAgentName(%q).String() = %q, want %q", in, n, in)
		}
	}
}

// TestTypedZeroValue documents the invariant that a zero-value TeamID /
// AgentName is invalid by construction: NewTeamID("") rejects empty, so if a
// caller forgot to check err the zero value is still empty and rejected by
// every downstream consumer that runs Validate* again as defense-in-depth.
func TestTypedZeroValue(t *testing.T) {
	var zt TeamID
	if zt.String() != "" {
		t.Fatalf("zero TeamID.String() = %q, want empty", zt)
	}
	if err := ValidateTeamName(zt.String()); err == nil {
		t.Fatal("ValidateTeamName(zero TeamID) accepted empty (should reject)")
	}
	var zn AgentName
	if zn.String() != "" {
		t.Fatalf("zero AgentName.String() = %q, want empty", zn)
	}
	if err := ValidateMemberName(zn.String()); err == nil {
		t.Fatal("ValidateMemberName(zero AgentName) accepted empty (should reject)")
	}
}
