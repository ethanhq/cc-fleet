// Package run implements `cc-fleet run <vendor>` (lane 0): launch an interactive
// foreground claude REPL backed by a vendor's profile + default model, by
// replacing the cc-fleet process via exec. It is a strict subset of the spawn
// pipeline — no tmux, team, locks, settle gate, or fingerprint recipe
// placeholders — and holds the same key-safety invariant (vendor auth flows
// only through the profile apiKeyHelper; no key in env/argv).
package run

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// Request is a lane-0 launch request. Model overrides the vendor's default_model
// when non-empty; PermissionMode is a validated permmode value — "" means the
// caller passed no permission flag, so Run falls back to the provider's configured
// default_permission (itself possibly "" → no permission flag); ExtraArgs are
// passed through to claude after cc-fleet's flags.
type Request struct {
	Vendor         string
	Model          string
	PermissionMode string
	ExtraArgs      []string
}

// reservedFlags are cc-fleet's own — it sets --settings / --model and the
// permission flags itself (before the passthrough). Repeating one in the
// passthrough would shadow the managed value, so it is rejected up front.
var reservedFlags = []string{"--settings", "--model", "--permission-mode", "--dangerously-skip-permissions"}

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

// execClaude replaces the current process with claude via execve. A seam so
// tests intercept the launch instead of replacing the test process. cc-fleet
// targets linux/darwin (like the rest of the binary, which uses unix-only
// flock/tmux), so syscall.Exec is always available.
var execClaude = func(bin string, argv, env []string) error {
	return syscall.Exec(bin, argv, env)
}

// Run validates the request, ensures the vendor profile, resolves the claude
// binary, and replaces the current process with an interactive claude bound to
// the vendor. Fail-before-exec: every rejecting check runs before any process
// replacement. On success it does not return.
func Run(req Request) error {
	if err := ids.ValidateVendorName(req.Vendor); err != nil {
		return err
	}
	for _, a := range req.ExtraArgs {
		if f := reservedFlag(a); f != "" {
			return fmt.Errorf("%s is managed by cc-fleet; use the run flag, not a passthrough arg", f)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load vendors.toml: %w", err)
	}
	v, ok := cfg.Vendors[req.Vendor]
	if !ok {
		return fmt.Errorf("vendor %q is not configured", req.Vendor)
	}
	if !v.Enabled {
		return fmt.Errorf("vendor %q is disabled", req.Vendor)
	}

	// Resolve model (capability keyword default/strong/fast → slot id, else a
	// literal id, "" → default_model).
	model := v.ResolveModel(req.Model)
	// config.Load guarantees a non-empty default_model; this guard is defensive.
	if model == "" {
		return fmt.Errorf("vendor %q has no default_model; pass --model", req.Vendor)
	}

	bin, err := resolveBinary()
	if err != nil {
		return err
	}

	// For a codex provider, ensure the conversion daemon is up before the profile
	// write (and before the exec that replaces this process — there is no
	// after-exec hook), fail-before-mutation.
	if err := codexproxy.EnsureForVendor(v, nil); err != nil {
		return fmt.Errorf("codex proxy unavailable: %w", err)
	}

	profilePath, err := profile.WriteForVendor(v, "")
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
	return execClaude(bin, argv, childenv.Clean(os.Environ()))
}

// buildArgv builds the claude argv: bin, cc-fleet's managed flags
// (--settings/--model + permission flags), then the passthrough. Managed flags
// go first so claude always parses them — a passthrough "--" or value flag can't
// push them past option parsing and drop the vendor profile. argv[0] == bin.
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
