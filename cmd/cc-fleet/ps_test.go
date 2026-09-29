package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// TestPsRowShape locks the ps --json envelope and row keys:
// tmux_socket_path replaces tmux_socket, the discovery-only Argv,
// ProcStart and SpawnTime never reach JSON, and the omitempty fields drop out
// of a minimal row.
func TestPsRowShape(t *testing.T) {
	full := teardown.Teammate{
		AgentID: "w@session-7c8f769b", Name: "w", Team: "session-7c8f769b", PaneID: "%3",
		Provider: "glm", Model: "glm-4.6", PID: 501, ProcStart: "123", Argv: []string{"claude", "--agent-id", "w@session-7c8f769b"},
		Socket: "/tmp/tmux-501/default", Backend: teardown.BackendTmux, LeadPID: 100, LeadSessionID: "s1",
		State: teardown.StateFailed, ErrorCode: "BAD_ARGS", Hidden: true, Legacy: true, SpawnTime: 1,
		Status: "error", ErrorClass: teardown.ClassLaunchFailed, Detail: "d",
	}
	data, err := json.Marshal(psEnvelope{OK: true, Teammates: []teardown.Teammate{full, {}}})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK        bool             `json:"ok"`
		Teammates []map[string]any `json:"teammates"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || len(env.Teammates) != 2 {
		t.Fatalf("envelope = %s", data)
	}
	wantFull := []string{"agent_id", "backend", "detail", "error_class", "error_code", "hidden", "lead_pid",
		"lead_session_id", "legacy", "model", "name", "pane_id", "pid", "provider", "state", "status", "team",
		"tmux_socket_path"}
	if got := discPsKeys(env.Teammates[0]); !reflect.DeepEqual(got, wantFull) {
		t.Fatalf("full row keys = %v\nwant %v", got, wantFull)
	}
	wantMin := []string{"agent_id", "backend", "model", "name", "pane_id", "pid", "provider", "state", "team"}
	if got := discPsKeys(env.Teammates[1]); !reflect.DeepEqual(got, wantMin) {
		t.Fatalf("minimal row keys = %v\nwant %v", got, wantMin)
	}
	if env.Teammates[0]["tmux_socket_path"] != "/tmp/tmux-501/default" {
		t.Fatalf("tmux_socket_path = %v", env.Teammates[0]["tmux_socket_path"])
	}

	empty, err := json.Marshal(psEnvelope{OK: false, Teammates: []teardown.Teammate{}, Error: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"ok":false,"teammates":[],"error":"boom"}` {
		t.Fatalf("error envelope = %s", empty)
	}
}

func discPsKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
