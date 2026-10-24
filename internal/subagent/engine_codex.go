package subagent

import (
	"errors"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
)

var errEngineUnimplemented = errors.New("engine not implemented")

// unimplementedEngine is the safe default an engine embeds until it writes its
// own methods: a call that can fail returns a classified failure, the rest a
// neutral value, none panics. Adding a method to Engine means adding it here,
// and every embedding engine keeps compiling.
type unimplementedEngine struct{}

var _ Engine = unimplementedEngine{}

func (unimplementedEngine) Resolve() (string, string, error) { return "", "", errEngineUnimplemented }

func (unimplementedEngine) GateProfile(requested, _ string) (string, string) { return requested, "" }

func (unimplementedEngine) Prepare(v *config.Provider, _ *diag.Logger) (string, *Result) {
	r := fail(ErrCodeFailed, errEngineUnimplemented.Error(), v.Name, "")
	return "", &r
}

func (unimplementedEngine) Argv(launchSpec) ([]string, string, error) {
	return nil, "", errEngineUnimplemented
}

// Env is empty, never nil: a nil exec.Cmd.Env inherits the parent's whole
// environment, credentials included.
func (unimplementedEngine) Env([]string, launchSpec) []string { return []string{} }

func (unimplementedEngine) Classify(s launchSpec, _, _ []byte, _ int, _ bool) Result {
	return fail(ErrCodeFailed, errEngineUnimplemented.Error(), s.req.Provider, "")
}

// codexEngine will run workers on the Codex CLI; it writes no methods yet.
type codexEngine struct{ unimplementedEngine }

var _ Engine = codexEngine{}
