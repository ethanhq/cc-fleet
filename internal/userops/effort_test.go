package userops

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// Covers Edit of the slot efforts, and List as the read-back.
func TestEdit_SlotEffort(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("ds")}, "")
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		name, wantStrong, wantFast, wantErr string
		req                                 EditRequest
	}{
		{"set both", "max", "low", "", EditRequest{StrongEffort: str("max"), FastEffort: str("low")}},
		{"nil keeps them", "max", "low", "", EditRequest{DefaultModel: str("ds-2")}},
		{"bad level is rejected", "max", "low", `fast_effort "fastest" invalid`, EditRequest{FastEffort: str("fastest")}},
		{"empty clears one", "", "low", "", EditRequest{StrongEffort: str("")}},
	} {
		c.req.Name = "ds"
		_, err := Edit(c.req)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("%s: Edit error = %v, want it to contain %q", c.name, err, c.wantErr)
			}
		} else if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		list, err := List()
		if err != nil {
			t.Fatal(err)
		}
		v := list.Providers[0]
		if v.StrongEffort != c.wantStrong || v.FastEffort != c.wantFast {
			t.Fatalf("%s: listed strong/fast effort = %q/%q, want %q/%q", c.name, v.StrongEffort, v.FastEffort, c.wantStrong, c.wantFast)
		}
	}
}

// Setting the first slot effort, or clearing the last, rewrites the profile: it
// blanks CLAUDE_CODE_EFFORT_LEVEL only while some effort is set.
func TestEdit_SlotEffortRewritesProfile(t *testing.T) {
	setupHome(t)
	seedConfig(t, []*config.Provider{anthropicProvider("ds")}, "")
	str := func(s string) *string { return &s }
	path, err := profile.ProfilePath("ds")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		fast    string
		blanked bool
	}{{"first effort", "low", true}, {"last cleared", "", false}} {
		if _, err := Edit(EditRequest{Name: "ds", FastEffort: str(c.fast)}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var back struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		if _, ok := back.Env["CLAUDE_CODE_EFFORT_LEVEL"]; ok != c.blanked {
			t.Errorf("%s: profile blanks CLAUDE_CODE_EFFORT_LEVEL = %v, want %v", c.name, ok, c.blanked)
		}
	}
}
