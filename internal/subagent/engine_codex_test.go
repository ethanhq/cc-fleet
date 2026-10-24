package subagent

import (
	"errors"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// codexEngine writes no methods yet: through the embedded default, every call
// that can fail returns a classified failure and none panics.
func TestCodexEngineSkeletonFailsClassified(t *testing.T) {
	var eng Engine = codexEngine{}
	spec := launchSpec{req: Request{Provider: "cx", PromptProfile: ProfileSlim}}

	if _, _, err := eng.Resolve(); !errors.Is(err, errEngineUnimplemented) {
		t.Errorf("Resolve err = %v, want errEngineUnimplemented", err)
	}
	if eff, dn := eng.GateProfile(ProfileSlim, "1.0.0"); eff != ProfileSlim || dn != "" {
		t.Errorf("GateProfile = (%q, %q), want the request back unchanged", eff, dn)
	}
	settings, failed := eng.Prepare(&config.Provider{Name: "cx"}, nil)
	if settings != "" || failed == nil || failed.OK || failed.ErrorCode != ErrCodeFailed || failed.Provider != "cx" {
		t.Errorf("Prepare = (%q, %+v), want a SUBAGENT_FAILED result", settings, failed)
	}
	if argv, sidecar, err := eng.Argv(spec); argv != nil || sidecar != "" || !errors.Is(err, errEngineUnimplemented) {
		t.Errorf("Argv = (%q, %q, %v), want errEngineUnimplemented", argv, sidecar, err)
	}
	if env := eng.Env([]string{"ANTHROPIC_API_KEY=host"}, spec); env == nil || len(env) != 0 {
		t.Errorf("Env = %#v, want empty and non-nil (nil inherits the host env)", env)
	}
	if res := eng.Classify(spec, []byte("out"), nil, 0, false); res.OK || res.ErrorCode != ErrCodeFailed || res.Provider != "cx" {
		t.Errorf("Classify = %+v, want a SUBAGENT_FAILED result", res)
	}
}
