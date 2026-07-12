// Package run implements `cc-fleet run <provider>` (lane 0): launch an interactive
// foreground claude REPL backed by a provider's profile + default model, handing
// the process over to claude (an execve on unix, a wait-on-child on Windows). It
// is a strict subset of the spawn pipeline — no tmux, team, locks, settle gate, or
// fingerprint recipe placeholders — and holds the same key-safety invariant
// (provider auth flows only through the profile apiKeyHelper; no key in env/argv).
package run

import (
	"fmt"
	"os"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/profile"
	"github.com/ethanhq/cc-fleet/internal/providerclass"
)

// Request is a lane-0 launch request. Model overrides the provider's default_model
// when non-empty; PermissionMode is a validated permmode value — "" means the
// caller passed no permission flag, so Run falls back to the provider's configured
// default_permission (itself possibly "" → no permission flag); NoProbe skips the
// pre-launch endpoint protocol check; ExtraArgs are passed through to claude after
// cc-fleet's flags.
type Request struct {
	Provider       string
	Model          string
	PermissionMode string
	NoProbe        bool
	ExtraArgs      []string
}

// reservedFlags are cc-fleet's own — it sets --settings / --model and the
// permission flags itself (before the passthrough). Repeating one in the
// passthrough would shadow the managed value, so it is rejected up front.
var reservedFlags = []string{"--settings", "--model", "--permission-mode", "--dangerously-skip-permissions"}

// sniffMessagesRoute and ensureDaemon are seams so tests stay hermetic: the
// suite must neither touch the network nor let EnsureForProvider start a
// daemon from os.Executable() — which, in a test, is the test binary itself.
var (
	sniffMessagesRoute = providerclass.MessagesRouteMissing
	ensureDaemon       = codexproxy.EnsureForProvider
)

// resolveBinary returns the live claude binary path via the same gate spawn and
// subagent use (bundled-or-cached recipe → resolve → validate). It is a seam so
// tests need no real claude on the box.
var resolveBinary = func() (string, error) {
	fp, err := fingerprint.LoadOrBundled()
	if err != nil {
		return "", fmt.Errorf("load fingerprint: %w", err)
	}
	bin, err := fingerprint.ResolveBinaryPath(fp)
	if err != nil {
		return "", err
	}
	fp.BinaryPath = bin
	if err := fingerprint.ValidateForRuntime(fp); err != nil {
		return "", err
	}
	return bin, nil
}

// Run validates the request, ensures the provider profile, resolves the claude
// binary, and hands the process over to an interactive claude bound to the
// provider. Fail-before-launch: every rejecting check runs before any process
// is launched. On success it does not return.
func Run(req Request) error {
	if err := ids.ValidateProviderName(req.Provider); err != nil {
		return err
	}
	for _, a := range req.ExtraArgs {
		if f := reservedFlag(a); f != "" {
			return fmt.Errorf("%s is managed by cc-fleet; use the run flag, not a passthrough arg", f)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load providers.toml: %w", err)
	}
	v, ok := cfg.Providers[req.Provider]
	if !ok {
		return fmt.Errorf("provider %q is not configured", req.Provider)
	}
	if !v.Enabled {
		return fmt.Errorf("provider %q is disabled", req.Provider)
	}

	// Resolve model (capability keyword default/strong/fast → slot id, else a
	// literal id, "" → default_model).
	model := v.ResolveModel(req.Model)
	// config.Load guarantees a non-empty default_model; this guard is defensive.
	if model == "" {
		return fmt.Errorf("provider %q has no default_model; pass --model", req.Provider)
	}

	bin, err := resolveBinary()
	if err != nil {
		return err
	}

	// Protocol guard: an Anthropic-protocol provider whose endpoint 404s
	// POST /v1/messages (an OpenAI-only endpoint added under the Anthropic
	// "Custom" option) would exec a claude that can only render its generic
	// "model may not exist" — cc-fleet leaves the process image at exec and can
	// never rewrite it, so this is the last point an actionable error exists.
	// The sniff trips only on that positive signal and fails open on everything
	// else. The error never echoes the base_url: a path segment may embed a
	// credential, and this text is exactly what a user pastes into an issue.
	if !req.NoProbe && v.EffectiveProtocol() == "" && sniffMessagesRoute(v.BaseURL) {
		return fmt.Errorf("provider %q is configured as an Anthropic-protocol provider, but its endpoint answered HTTP 404 to POST /v1/messages — it likely speaks the OpenAI protocol; re-add the provider with an OpenAI protocol, or pass --no-probe to skip this check",
			req.Provider)
	}

	// For a codex provider, ensure the conversion daemon is up before the profile
	// write (and before the exec that replaces this process — there is no
	// after-exec hook), fail-before-mutation.
	if err := ensureDaemon(v, nil); err != nil {
		return fmt.Errorf("codex proxy unavailable: %w", err)
	}

	profilePath, err := profile.WriteForProvider(v, "")
	if err != nil {
		return fmt.Errorf("write profile: %w", err)
	}

	// An explicit --permission-mode / --dangerously-skip-permissions (resolved by
	// the caller into a non-empty PermissionMode) wins; otherwise fall back to the
	// provider's default_permission ("" → no permission flag, Claude's default mode).
	permMode := req.PermissionMode
	if permMode == "" {
		permMode = v.DefaultPermission
	}
	argv := buildArgv(bin, profilePath, model, permmode.ExplicitFlags(permMode), req.ExtraArgs)
	return execClaude(v, bin, argv, childenv.Clean(os.Environ()))
}

// buildArgv builds the claude argv: bin, cc-fleet's managed flags
// (--settings/--model + permission flags), then the passthrough. Managed flags
// go first so claude always parses them — a passthrough "--" or value flag can't
// push them past option parsing and drop the provider profile. argv[0] == bin.
func buildArgv(bin, profilePath, model string, permFlags, extra []string) []string {
	argv := []string{bin, "--settings", profilePath, "--model", model}
	argv = append(argv, permFlags...)
	return append(argv, extra...)
}

// reservedFlag returns the reserved flag a matches ("--model" / "--settings"),
// accepting both "--flag" and "--flag=value" forms; "" if a is not reserved.
func reservedFlag(a string) string {
	for _, f := range reservedFlags {
		if a == f || strings.HasPrefix(a, f+"=") {
			return f
		}
	}
	return ""
}
