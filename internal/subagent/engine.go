package subagent

import (
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
)

// launchSpec carries one run's resolved inputs to the engine.
type launchSpec struct {
	req      Request
	provider *config.Provider // nil for the native leaf
	bin      string           // Resolve's path
	settings string           // Prepare's output; "" for the native leaf
	model    string
	effort   string
	profile  string // effective prompt profile (GateProfile's output)
	jobID    string
}

// Engine is the CLI a worker runs on. Run and launchBackground own the shared
// pipeline (provider lookup, job registration, exec, cancel/overflow); an Engine
// owns only what differs per CLI.
type Engine interface {
	// Resolve is the binary gate: the engine CLI's path and version.
	Resolve() (path, version string, err error)
	// GateProfile maps a requested prompt profile to the one this CLI version can run.
	GateProfile(requested, version string) (effective, downgrade string)
	// Prepare readies provider-side state after the gate and before launch.
	Prepare(v *config.Provider, dg *diag.Logger) (settings string, failed *Result)
	// Argv builds the child argv; sidecar is an engine-owned file the caller
	// removes when the job never registers.
	Argv(s launchSpec) (argv []string, sidecar string, err error)
	Env(host []string, s launchSpec) []string
	// Classify reads a sync run's output. A background job's terminal state is
	// still classified from its meta by StatusFor, which assumes claude.
	Classify(s launchSpec, stdout, stderr []byte, exitCode int, timedOut bool) Result
}

// defaultEngine is the engine every worker runs on until engine selection exists.
var defaultEngine Engine = claudeEngine{}
