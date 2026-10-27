package subagent

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/profile"
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

var errCodexNotFound = errors.New("codex binary not found")

// codexVersions caches path → version once per path per process: a workflow
// resolves per leaf, and `codex --version` boots node on an npm install.
var codexVersions sync.Map

// codexEngine will run workers on the Codex CLI; so far it writes Resolve and
// Prepare. It must not become selectable before the conversion daemon serves
// /v1/responses: until then the CODEX_HOME Prepare writes points codex at a
// route that answers 404.
type codexEngine struct{ unimplementedEngine }

var _ Engine = codexEngine{}

// Resolve finds codex on PATH (codex has no per-version layout). The version is
// "" when unknown, not an error, and an unknown one is not cached: a cold node
// start can time out once.
func (codexEngine) Resolve() (string, string, error) {
	p, err := exec.LookPath("codex")
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errCodexNotFound, err)
	}
	// LookPath can return a relative path under GODEBUG=execerrdot=0.
	if abs, absErr := filepath.Abs(p); absErr == nil {
		p = abs
	}
	if !ccver.IsExecutable(p) {
		return "", "", fmt.Errorf("%w: %s is not a runnable file", errCodexNotFound, p)
	}
	if v, ok := codexVersions.Load(p); ok {
		return p, v.(string), nil
	}
	v := ccver.VersionForPath(p)
	if v != "" {
		codexVersions.Store(p, v)
	}
	return p, v, nil
}

// Prepare ensures the provider's conversion daemon, then writes its CODEX_HOME:
// a config.toml pointing codex at the daemon's Responses route, with
// `cc-fleet keyget` as the auth command so no key reaches env or argv. Direct
// (Anthropic-native) rows have no daemon to point at yet.
func (codexEngine) Prepare(v *config.Provider, dg *diag.Logger) (string, *Result) {
	if !v.DaemonBacked() {
		r := fail(ErrCodeFailed, fmt.Sprintf("provider %s talks to its endpoint directly, which the codex engine does not support yet", v.Name), v.Name, "")
		return "", &r
	}
	if err := ensureProviderProxy(v, dg); err != nil {
		r := fail(ErrCodeProxyUnavailable, err.Error(), v.Name, suggestionFor(ErrCodeProxyUnavailable))
		return "", &r
	}
	home, err := profile.WriteCodexHome(v, "")
	if err != nil {
		r := fail(ErrCodeFailed, fmt.Sprintf("write codex home for %s: %v", v.Name, err), v.Name, "")
		return "", &r
	}
	dg.Logf("subagent: codex home written %s", home)
	return home, nil
}
