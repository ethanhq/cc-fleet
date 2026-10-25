package subagent

import (
	"fmt"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/diag"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// claudeEngine runs workers on Claude Code.
type claudeEngine struct{}

func (claudeEngine) Resolve() (string, string, error) { return resolveBinaryPathVersion() }

func (claudeEngine) GateProfile(requested, version string) (string, string) {
	return gateProfile(requested, version, nil)
}

// Prepare ensures the conversion daemon (daemon-backed providers), then the
// provider profile. MUST run after the binary gate: a missing claude never
// leaves a profile behind, and a daemon failure leaves none either. The profile
// write is atomic and idempotent, so N concurrent subagents need no lock.
func (claudeEngine) Prepare(v *config.Provider, dg *diag.Logger) (string, *Result) {
	if err := ensureProviderProxy(v, dg); err != nil {
		r := fail(ErrCodeProxyUnavailable, err.Error(), v.Name, suggestionFor(ErrCodeProxyUnavailable))
		return "", &r
	}
	profilePath, err := profile.WriteForProvider(v, "")
	if err != nil {
		r := fail(ErrCodeFailed, fmt.Sprintf("write profile for %s: %v", v.Name, err), v.Name, "")
		return "", &r
	}
	dg.Logf("subagent: profile written %s", profilePath)
	return profilePath, nil
}

func (claudeEngine) Argv(s launchSpec) ([]string, string, error) {
	slim, err := buildSlimArgv(s.profile, s.jobID, s.req, s.model)
	if err != nil {
		return nil, "", err
	}
	return buildArgv(s.bin, s.settings, s.model, s.effort, s.req, slim), slim.promptFile, nil
}

func (claudeEngine) Env(host []string, s launchSpec) []string {
	env := childenv.WithWorkerEnv(childenv.Clean(host))
	if s.profile == ProfileSlimRO {
		env = append(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
	}
	return env
}

// Classify reads claude's result envelope; a stream-json run classifies only its
// terminal type:"result" line (byte-identical to --output-format json), not the
// whole multi-line transcript.
func (claudeEngine) Classify(s launchSpec, stdout, stderr []byte, exitCode int, timedOut bool) Result {
	innerJSON := s.req.JSON || s.req.OutputFormat == "json" || s.req.StreamActivity
	out := stdout
	if s.req.StreamActivity {
		out = extractResultLine(stdout)
	}
	return classify(s.req, s.model, out, stderr, exitCode, timedOut, innerJSON)
}
