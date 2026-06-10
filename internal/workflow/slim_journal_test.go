package workflow

import (
	"testing"

	"github.com/ethanhq/cc-fleet/internal/subagent"
)

// fullKey is the journal key for a full-profile leaf (the slim fields don't fold), used
// as the back-compat baseline below. "full" and "" are interchangeable here (the fold
// guard treats both as full).
func fullKey(provider, model, prompt, schema, isolation string) string {
	return journalKey(provider, model, prompt, schema, isolation, "full", nil, false, false)
}

// TestJournalKeyFullByteIdentical pins the full-profile key for a fixed input to the
// value today's 5-field scheme produces — a golden so a slim-folding change can never
// silently shift an existing saved-run key (which would invalidate every cached leaf on
// resume). The hex is the v1 + length-prefixed (provider,model,prompt,schema,isolation)
// preimage; "" and "full" effective profiles must both reproduce it, and the slim fields
// must not fold for them.
func TestJournalKeyFullByteIdentical(t *testing.T) {
	const golden = "910fc45a500548544dff296be054eedbd17c82de49a3f92baadb550623163cce"
	provider, model := "deepseek", "deepseek-chat"
	prompt, schema := "do the thing", `{"required":["answer"]}`
	// "" and "full" effective profiles, and every slim-field combination under them, must
	// all reproduce the golden — full keys are byte-identical to today regardless.
	for _, eff := range []string{"", "full"} {
		for _, tools := range [][]string{nil, {"Read"}, {"Bash", "Grep"}} {
			for _, noSkills := range []bool{false, true} {
				for _, mcp := range []bool{false, true} {
					got := journalKey(provider, model, prompt, schema, "", eff, tools, noSkills, mcp)
					if got != golden {
						t.Fatalf("full key drifted: profile=%q tools=%v noSkills=%v mcp=%v → %s, want golden %s",
							eff, tools, noSkills, mcp, got, golden)
					}
				}
			}
		}
	}
}

// TestJournalKeySlimFolds: a slim effective profile folds its determinants, so a slim leaf
// keys differently from the same full leaf; the tool set is order-insensitive (canonicalized
// before keying); and flipping skills or mcp changes the key.
func TestJournalKeySlimFolds(t *testing.T) {
	base := fullKey("v", "m", "p", "", "")
	slim := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, false, false)
	if slim == base {
		t.Fatal("a slim leaf must key differently from the same full leaf")
	}
	slimRO := journalKey("v", "m", "p", "", "", "slim-ro", []string{"Bash", "Read"}, false, false)
	if slimRO == slim {
		t.Fatal("slim and slim-ro must key differently (different effective profile)")
	}

	// Tool order is irrelevant: the engine canonicalizes (dedupe+sort) before keying, so
	// the key folds the canonical join — caller order never shifts it.
	a := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, false, false)
	b := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, false, false)
	if a != b {
		t.Fatal("the same canonical tool set must produce the same key")
	}
	diffTools := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Grep"}, false, false)
	if diffTools == slim {
		t.Fatal("a different tool set must change the key")
	}

	// skills= and mcp= flips each change the key.
	noSkills := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, true, false)
	if noSkills == slim {
		t.Fatal("flipping skills off must change the key")
	}
	withMCP := journalKey("v", "m", "p", "", "", "slim", []string{"Bash", "Read"}, false, true)
	if withMCP == slim {
		t.Fatal("flipping mcp on must change the key")
	}
}

// TestJournalKeyResolvedDefaultTools: a bare slim leaf (tools= omitted) and a leaf that
// passes the profile DEFAULT set explicitly must key identically — agent() resolves the
// default set BEFORE keying (and feeds the same set to the leaf), so the two are the same
// run and a resume serves one for the other. Guards the F1 divergence where a bare slim
// leaf keyed with nil tools while executing DefaultSlimTools.
func TestJournalKeyResolvedDefaultTools(t *testing.T) {
	for _, profile := range []string{subagent.ProfileSlim, subagent.ProfileSlimRO} {
		for _, noSkills := range []bool{false, true} {
			resolved, err := subagent.CanonicalizeTools(subagent.DefaultSlimTools(profile, noSkills))
			if err != nil {
				t.Fatalf("canonicalize default tools for %q: %v", profile, err)
			}
			bare := journalKey("v", "m", "p", "", "", profile, resolved, noSkills, false)
			explicit := journalKey("v", "m", "p", "", "", profile, resolved, noSkills, false)
			if bare != explicit {
				t.Fatalf("%q noSkills=%v: bare-default and explicit-default keys differ", profile, noSkills)
			}
			// And the resolved default set must differ from the nil set agent() used to fold
			// (the bug): so a bare slim leaf is NOT keyed as if it carried no tools.
			if nilKey := journalKey("v", "m", "p", "", "", profile, nil, noSkills, false); nilKey == bare {
				t.Fatalf("%q noSkills=%v: a resolved-default key must differ from a nil-tools key", profile, noSkills)
			}
		}
	}
}
