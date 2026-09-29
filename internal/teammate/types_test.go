package teammate

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseAgentType(t *testing.T) {
	type want struct {
		t    AgentType
		ours bool
		err  bool
	}
	cases := []struct {
		in string
		want
	}{
		// ours, valid
		{"ccf-p", want{AgentType{"p", SlotDefault}, true, false}},
		{"ccf-p.strong", want{AgentType{"p", SlotStrong}, true, false}},
		{"ccf-p.fast", want{AgentType{"p", SlotFast}, true, false}},
		{"ccf-openrouter", want{AgentType{"openrouter", SlotDefault}, true, false}},
		{"ccf-my_prov-2.fast", want{AgentType{"my_prov-2", SlotFast}, true, false}},
		// ours, bad slot
		{"ccf-p.bogus", want{ours: true, err: true}},
		{"ccf-p.default", want{ours: true, err: true}}, // only strong/fast may follow the dot
		{"ccf-p.", want{ours: true, err: true}},
		{"ccf-p.STRONG", want{ours: true, err: true}},
		// ours, empty or invalid provider name
		{"ccf-", want{ours: true, err: true}},
		{"ccf-.strong", want{ours: true, err: true}},
		{"ccf-1abc", want{ours: true, err: true}},
		{"ccf-a/b", want{ours: true, err: true}},
		{"ccf-a b", want{ours: true, err: true}},
		{"ccf-a.b.strong", want{ours: true, err: true}},
		{"ccf-" + strings.Repeat("a", 33), want{ours: true, err: true}},
		// ours, reserved native provider
		{"ccf-claude", want{ours: true, err: true}},
		{"ccf-claude.strong", want{ours: true, err: true}},
		// not ours: native path
		{"general-purpose", want{}},
		{"", want{}},
		{"ccf", want{}},
		{"CCF-p", want{}},
		{"claude", want{}},
		{"x-ccf-p", want{}},
	}
	for _, tc := range cases {
		got, ours, err := ParseAgentType(tc.in)
		if ours != tc.ours || (err != nil) != tc.err || got != tc.t {
			t.Errorf("ParseAgentType(%q) = (%+v, %v, %v); want (%+v, %v, err=%v)",
				tc.in, got, ours, err, tc.t, tc.ours, tc.err)
			continue
		}
		if ours && err == nil {
			if s := got.String(); s != tc.in {
				t.Errorf("ParseAgentType(%q).String() = %q, want the input back", tc.in, s)
			}
		}
	}
}

func TestAgentTypeString(t *testing.T) {
	for _, tc := range []struct {
		t    AgentType
		want string
	}{
		{AgentType{"p", SlotDefault}, "ccf-p"},
		{AgentType{"p", ""}, "ccf-p"}, // zero slot = default
		{AgentType{"p", SlotStrong}, "ccf-p.strong"},
		{AgentType{"p", SlotFast}, "ccf-p.fast"},
	} {
		if got := tc.t.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.t, got, tc.want)
		}
	}
}

func TestFailureLineRoundTrip(t *testing.T) {
	const id = "worker-1@session-7c8f769b"
	line := FormatFailureLine(CodeProviderDisabled, id, "provider glm is disabled", "run cc-fleet edit glm")
	if want := "cc-fleet teammate: PROVIDER_DISABLED: worker-1@session-7c8f769b: provider glm is disabled — run cc-fleet edit glm"; line != want {
		t.Fatalf("FormatFailureLine = %q\nwant              %q", line, want)
	}

	codes := []string{
		CodeLaneUnavailable, CodeSetupRequired, CodeLeadRestart, CodeModeInProcess, CodeBadAgentCall,
		CodeClaudeNotFound, CodeAmbiguousTarget, CodeIdentityMismatch, CodeSettingsUnparseable,
		CodeSetupConflict, CodeCommandRemoved, CodeBadArgs, CodeUnsupportedOnWindows, CodeUnknownProvider,
		CodeProviderDisabled, CodeConfigLoadFailed, CodeProfileWriteFailed, CodeCodexProxyUnavailable,
		CodeProviderUnreachable, CodeKeyInvalid, CodeInternal,
	}
	msgs := []string{
		"plain message",
		"message: with colons: and — dashes", // later separators must not confuse the parser
		"multi\nline\r\nmessage",             // flattened to one line
		"",
	}
	for _, code := range codes {
		for _, msg := range msgs {
			line := FormatFailureLine(code, id, msg, "next: step\nhere")
			if strings.ContainsAny(line, "\r\n") {
				t.Fatalf("FormatFailureLine(%s, %q) is not one line: %q", code, msg, line)
			}
			gotCode, gotID, ok := ParseFailureLine(line)
			if !ok || gotCode != code || gotID != id {
				t.Fatalf("ParseFailureLine(%q) = (%q, %q, %v), want (%q, %q, true)", line, gotCode, gotID, ok, code, id)
			}
		}
	}

	// The shim's own fallback line is parsed the same way.
	shimLine := "cc-fleet teammate: TEAMMATE_SETUP_REQUIRED: w@session-1234abcd: launcher_target_missing (/x/cc-fleet) — run: cc-fleet repair"
	if c, a, ok := ParseFailureLine(shimLine); !ok || c != CodeSetupRequired || a != "w@session-1234abcd" {
		t.Fatalf("ParseFailureLine(shim line) = (%q, %q, %v)", c, a, ok)
	}

	for _, bad := range []string{
		"",
		"cc-fleet teammate: bad code: a@b: x",   // code must be [A-Z_]+
		"cc-fleet teammate: INTERNAL: : x",      // empty agent id
		"cc-fleet teammate: INTERNAL: a b: x",   // whitespace in agent id
		"cc-fleet teammate: INTERNAL: a@b",      // no ": " after the agent id
		" cc-fleet teammate: INTERNAL: a@b: x",  // anchored at line start
		"cc-fleet: INTERNAL: a@b: x",            // wrong prefix
		"xcc-fleet teammate: INTERNAL: a@b: x",  // wrong prefix
		"cc-fleet teammate:  INTERNAL: a@b: x ", // double space
	} {
		if c, a, ok := ParseFailureLine(bad); ok {
			t.Errorf("ParseFailureLine(%q) = (%q, %q, true), want no match", bad, c, a)
		}
	}
}

// jsonKeys returns the sorted top-level keys of a JSON object.
func jsonKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeys(keys ...string) []string {
	sort.Strings(keys)
	return keys
}

// decodeStrictRoundTrip decodes doc into v rejecting unknown fields, re-encodes
// it and requires the result to be semantically equal to doc: the struct's
// field names and omitempty rules reproduce the documented example exactly.
func decodeStrictRoundTrip(t *testing.T, doc string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s: %v", doc, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(doc), &want); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the document:\n got %s\nwant %s", out, doc)
	}
}

func TestResultJSONShape(t *testing.T) {
	full := Result{
		OK: true, Protocol: Protocol, Provider: "p", Slot: SlotStrong, Model: "m", AgentType: "ccf-p.strong",
		Team: "session-7c8f769b", LeadPID: 4242, CCVersion: "2.1.281", Entrypoint: "cli",
		TeammateMode: "tmux", TeammateModeSource: SourceCLI, BackendHint: HintTmux, Launcher: "/x/claude-teammate",
		Warnings: []string{WarnAutoMayFallback}, ErrorCode: CodeInternal, Detail: "d", ErrorMsg: "e", Suggestion: "s",
	}
	data, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wantAll := sortedKeys("ok", "protocol", "provider", "slot", "model", "agent_type", "team", "lead_pid",
		"cc_version", "entrypoint", "teammate_mode", "teammate_mode_source", "backend_hint", "launcher",
		"warnings", "error_code", "detail", "error_msg", "suggestion")
	if got := jsonKeys(t, data); !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("Result keys = %v\nwant        %v", got, wantAll)
	}

	// A failure carries ok=false, protocol and warnings:[] even when empty.
	fail := Result{Protocol: Protocol, Warnings: []string{}, ErrorCode: CodeLaneUnavailable,
		Detail: DetailNoSessionTeam, ErrorMsg: "e", Suggestion: "s"}
	data, err = json.Marshal(fail)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := jsonKeys(t, data), sortedKeys("ok", "protocol", "warnings", "error_code", "detail", "error_msg", "suggestion"); !reflect.DeepEqual(got, want) {
		t.Fatalf("failure Result keys = %v, want %v", got, want)
	}
	for _, frag := range []string{`"ok":false`, `"protocol":1`, `"warnings":[]`} {
		if !bytes.Contains(data, []byte(frag)) {
			t.Fatalf("failure Result %s lacks %s", data, frag)
		}
	}

	// The check and setup envelope examples decode strictly and re-encode unchanged.
	decodeStrictRoundTrip(t, `{"ok":true,"protocol":1,"provider":"openrouter","slot":"default","model":"deepseek/x[1m]","agent_type":"ccf-openrouter",
 "team":"session-7c8f769b","lead_pid":4242,"cc_version":"2.1.281","entrypoint":"cli",
 "teammate_mode":"tmux","teammate_mode_source":"cli","backend_hint":"tmux",
 "launcher":"/Users/x/.config/cc-fleet/bin/claude-teammate","warnings":[]}`, &Result{})
	decodeStrictRoundTrip(t, `{"ok":false,"protocol":1,"warnings":[],"error_code":"TEAMMATE_LANE_UNAVAILABLE","detail":"no_session_team",
 "error_msg":"this Claude Code session (entrypoint=claude-desktop) has no agent team","suggestion":"use cc-fleet subagent"}`, &Result{})
	decodeStrictRoundTrip(t, `{"ok":true,"action":"setup","shim":"/Users/x/.config/cc-fleet/bin/claude-teammate","settings_path":"/Users/x/.claude/settings.json",
 "changed":["env.CLAUDE_CODE_TEAMMATE_COMMAND"],"agent_defs":{"written":["ccf-openrouter.md"],"removed":[],"unchanged":[]},
 "teammate_mode":"auto","teammate_mode_source":"userSettings","mode_written":false,"restart_required":true,"warnings":[]}`, &SetupResult{})
}
