package leadsession

// Session is a read-only mirror of Claude Code's sessions/<pid>.json registry
// entry. Claude Code writes the file; cc-fleet only reads it.
type Session struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Cwd        string `json:"cwd"`
	StartedAt  int64  `json:"startedAt"` // unix ms
	ProcStart  string `json:"procStart,omitempty"`
	Version    string `json:"version,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
}
