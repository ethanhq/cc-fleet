package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/onboarding"
)

// hideClaude makes ccver.Located fail for the test: the fresh hermetic HOME that
// setupEnv installs has no per-version layout, and this empties PATH. Call after
// setupEnv(t).
func hideClaude(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestNewModel_InstallClaudeGating(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	if got := NewModel().screen; got != screenInstallClaude {
		t.Fatalf("NewModel screen = %d, want screenInstallClaude (claude absent, no ack)", got)
	}
	if err := (onboarding.State{ClaudeInstallAck: true}).Save(); err != nil {
		t.Fatal(err)
	}
	if got := NewModel().screen; got == screenInstallClaude {
		t.Fatalf("after ClaudeInstallAck the install nudge must not show (got screen %d)", got)
	}
}

func TestUpdateInstallClaude_Navigate(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	m := Model{screen: screenInstallClaude}
	m, _ = press(t, m, "down")
	if m.installCursor != 1 {
		t.Fatalf("after down: cursor=%d, want 1", m.installCursor)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // clamp at the last option
	if m.installCursor != installOptionCount-1 {
		t.Fatalf("cursor=%d, want clamp at %d", m.installCursor, installOptionCount-1)
	}
	m, _ = press(t, m, "up")
	if m.installCursor != 1 {
		t.Fatalf("after up: cursor=%d, want 1", m.installCursor)
	}
}

func TestUpdateInstallClaude_ManualAcksAndLeaves(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	m := Model{screen: screenInstallClaude, installCursor: 1} // "I'll install it myself"
	m, _ = press(t, m, "enter")
	if m.screen == screenInstallClaude {
		t.Fatal("manual choice should leave the install nudge")
	}
	if st, _ := onboarding.LoadState(); !st.ClaudeInstallAck {
		t.Fatal("manual choice must ack so the offer never returns")
	}
}

func TestUpdateInstallClaude_LaterDoesNotAck(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	m := Model{screen: screenInstallClaude, installCursor: 2} // "later"
	m, _ = press(t, m, "enter")
	if m.screen == screenInstallClaude {
		t.Fatal("later should leave the install nudge")
	}
	if st, _ := onboarding.LoadState(); st.ClaudeInstallAck {
		t.Fatal("later must NOT ack — the offer returns next launch")
	}
}

func TestUpdateInstallClaude_EscDoesNotAck(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	m := Model{screen: screenInstallClaude}
	m, _ = press(t, m, "esc")
	if m.screen == screenInstallClaude {
		t.Fatal("esc should leave the install nudge")
	}
	if st, _ := onboarding.LoadState(); st.ClaudeInstallAck {
		t.Fatal("esc (later) must NOT ack")
	}
}

func TestUpdateInstallClaude_OutcomeAnyKeyProceeds(t *testing.T) {
	setupEnv(t)
	m := Model{screen: screenInstallClaude, installMsg: "Claude Code installed ✓"}
	m, _ = press(t, m, "enter")
	if m.screen == screenInstallClaude {
		t.Fatal("with an outcome shown, any key must proceed past the nudge")
	}
}

func TestApplyClaudeInstallResult_SuccessAcks(t *testing.T) {
	setupEnv(t) // PATH still carries TestMain's stub claude → Located() succeeds
	m := Model{screen: screenInstallClaude}.applyClaudeInstallResult(nil)
	if !strings.Contains(m.installMsg, "installed") {
		t.Fatalf("installMsg = %q, want a success line", m.installMsg)
	}
	if st, _ := onboarding.LoadState(); !st.ClaudeInstallAck {
		t.Fatal("a located post-install must ack")
	}
}

func TestApplyClaudeInstallResult_FailureNoAck(t *testing.T) {
	setupEnv(t)
	hideClaude(t)
	m := Model{screen: screenInstallClaude}.applyClaudeInstallResult(errors.New("boom"))
	if !strings.Contains(m.installMsg, "didn't complete") {
		t.Fatalf("installMsg = %q, want a failure line", m.installMsg)
	}
	if st, _ := onboarding.LoadState(); st.ClaudeInstallAck {
		t.Fatal("a failed install must NOT ack")
	}
}

func TestInstallClaudeView_Wording(t *testing.T) {
	v := Model{screen: screenInstallClaude}.viewInstallClaude()
	for _, want := range []string{"cc-fleet · setup", "↑/↓ move · enter select · esc later", "install it for me", "isn't installed"} {
		if !strings.Contains(v, want) {
			t.Errorf("install-Claude view missing %q", want)
		}
	}
}
