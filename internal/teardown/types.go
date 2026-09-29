package teardown

// Teammate is the structured row cc-fleet ps emits per provider teammate.
// JSON tags are the `ps --json` contract — keep stable.
//
// Status / ErrorClass / Detail are health fields populated ONLY by
// `cc-fleet ps --check`; they are omitempty, so a plain `ps --json` omits them.
// ProcStart and Argv never reach JSON: teardown uses them to re-verify a
// process's identity before killing it.
type Teammate struct {
	AgentID       string   `json:"agent_id"`
	Name          string   `json:"name"`
	Team          string   `json:"team"`
	PaneID        string   `json:"pane_id"`
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	PID           int      `json:"pid"`
	ProcStart     string   `json:"-"`
	Argv          []string `json:"-"`                          // exact argv read at discovery; teardown compares it element-wise with Cmdline(pid) before killing
	Socket        string   `json:"tmux_socket_path,omitempty"` // breaking rename: 0.3.x emitted tmux_socket (a -L name); now the absolute path for -S
	Backend       string   `json:"backend"`                    // tmux | in-process | unknown (iTerm2 native splits report unknown)
	LeadPID       int      `json:"lead_pid,omitempty"`
	LeadSessionID string   `json:"lead_session_id,omitempty"`
	State         string   `json:"state"`                // running | orphaned | failed | bypassed
	ErrorCode     string   `json:"error_code,omitempty"` // only when failed
	Hidden        bool     `json:"hidden,omitempty"`
	Legacy        bool     `json:"legacy,omitempty"` // true only on independent positive evidence: a cc-fleet-swarm-* socket, or 0.3.x-only leadSessionId/tmuxSocket member keys
	SpawnTime     int64    `json:"-"`
	Status        string   `json:"status,omitempty"`      // --check only: ok | error | unknown
	ErrorClass    string   `json:"error_class,omitempty"` // + launch_failed | launcher_bypassed
	Detail        string   `json:"detail,omitempty"`
}

const (
	StateRunning  = "running"
	StateOrphaned = "orphaned"
	StateFailed   = "failed"
	StateBypassed = "bypassed"

	BackendTmux      = "tmux"
	BackendInProcess = "in-process"
	BackendUnknown   = "unknown"

	ClassLaunchFailed     = "launch_failed"
	ClassLauncherBypassed = "launcher_bypassed"
)
