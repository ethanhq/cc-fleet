package main

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/panevis"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// TestTeardownWindowsCode: on Windows teardown refuses with
// UNSUPPORTED_ON_WINDOWS (it used to report INTERNAL), before parsing or
// discovery, in the regular envelope shape.
func TestTeardownWindowsCode(t *testing.T) {
	orig := onWindows
	t.Cleanup(func() { onWindows = orig })
	onWindows = true

	res := runTeardown("%3", "", nil)
	if res.OK || res.ErrorCode != "UNSUPPORTED_ON_WINDOWS" {
		t.Fatalf("result = %+v", res)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if env["target"] != "%3" || env["killed"] == nil || env["skipped"] == nil || env["error_msg"] == "" {
		t.Fatalf("envelope = %s", data)
	}
}

// TestTeardownBadTarget: a target that is neither %N, name@team nor a team is
// BAD_ARGS with the accepted forms as suggestion.
func TestTeardownBadTarget(t *testing.T) {
	orig := onWindows
	t.Cleanup(func() { onWindows = orig })
	onWindows = false

	for _, arg := range []string{"alpha/worker", "%x", "../x@alpha"} {
		res := runTeardown(arg, "", nil)
		if res.OK || res.ErrorCode != teardown.ErrCodeBadArgs || res.Suggestion == "" {
			t.Fatalf("%q: result = %+v", arg, res)
		}
	}
}

// TestKillCommandsTakeSocket: teardown, hide and show accept --socket.
func TestKillCommandsTakeSocket(t *testing.T) {
	for _, cmd := range []*cobra.Command{newTeardownCmd(), newHideCmd(), newShowCmd()} {
		if cmd.Flags().Lookup("socket") == nil || cmd.Flags().Lookup("json") == nil {
			t.Fatalf("%s lacks --socket or --json", cmd.Name())
		}
	}
}

// TestHideShowWindowsCode: hide and show refuse on Windows.
func TestHideShowWindowsCode(t *testing.T) {
	orig := onWindows
	t.Cleanup(func() { onWindows = orig })
	onWindows = true
	for _, hide := range []bool{true, false} {
		if res := runPaneVis("%3", "", hide); res.OK || res.ErrorCode != panevis.ErrUnsupportedOnWindows {
			t.Fatalf("hide=%v: result = %+v", hide, res)
		}
	}
}
