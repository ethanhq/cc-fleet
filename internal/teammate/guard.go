package teammate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// hookInput is the part of Claude Code's PreToolUse JSON the guard decodes.
type hookInput struct {
	ToolName  string    `json:"tool_name"`
	ToolInput AgentCall `json:"tool_input"`
}

// Guard is the PreToolUse(Agent) hook verdict. It returns the process exit
// code: 0 = allowed or a deny was printed; 1 = internal error; 3 = unsupported
// protocol. hooks/teammate-guard.sh turns 1 and 3 into a block. An allowed call
// prints nothing, so Claude Code's normal permission flow still runs. Guard
// reads and writes no files beyond what Evaluate's read-only steps read.
func Guard(ctx context.Context, protocol int, stdin io.Reader, stdout io.Writer) int {
	if protocol != Protocol {
		return 3
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return 1
	}
	var hi hookInput
	if err := json.Unmarshal(data, &hi); err != nil {
		return 1
	}
	call := hi.ToolInput
	if hi.ToolName != "Agent" || !strings.HasPrefix(call.SubagentType, TypePrefix) {
		return 0
	}

	var r Result
	if t, _, err := ParseAgentType(call.SubagentType); err != nil {
		r = evalFail(CodeBadArgs, "", err.Error(), "use a `ccf-<provider>[.strong|.fast]` type for a configured provider (`cc-fleet list`)")
		if isReservedType(call.SubagentType) {
			r = evalFail(CodeBadArgs, "", "`claude` is reserved for native Claude teammates",
				"use a native agent type; cc-fleet is not needed")
		}
	} else {
		r = Evaluate(ctx, Input{Provider: t.Provider, Slot: t.Slot, AgentCall: &call})
	}
	if r.OK {
		return 0
	}
	if err := writeDeny(stdout, r); err != nil {
		return 1
	}
	return 0
}

// isReservedType reports whether s is ccf-claude with or without a slot suffix.
func isReservedType(s string) bool {
	rest := strings.TrimPrefix(s, TypePrefix)
	provider, _, _ := strings.Cut(rest, ".")
	return provider == config.ReservedNativeProvider
}

// writeDeny prints the one-line PreToolUse deny decision for r.
func writeDeny(w io.Writer, r Result) error {
	code := r.ErrorCode
	if r.Detail != "" {
		code += "(" + r.Detail + ")"
	}
	var out struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = fmt.Sprintf("cc-fleet: %s: %s — %s", code, r.ErrorMsg, r.Suggestion)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
