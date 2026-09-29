package teammate

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/claudebin"
	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/leadsession"
	"github.com/ethanhq/cc-fleet/internal/profile"
)

// Test seams. The launcher never forks: a successful route or passthrough
// replaces this process, so execFn only returns on failure (or from a stub).
var (
	execFn              = syscall.Exec
	ensureDaemonFn      = func(v *config.Provider) error { return codexproxy.EnsureForProvider(v, nil) }
	writeProfileFn      = func(v *config.Provider) (string, error) { return profile.WriteForProvider(v, "") }
	resolveLeadBinaryFn = resolveLeadBinary
)

// The argv flags the launcher reads or rewrites; each is accepted as
// `--x v` and `--x=v` (design F32).
const (
	flagAgentID         = "--agent-id"
	flagAgentType       = "--agent-type"
	flagParentSessionID = "--parent-session-id"
	flagSettings        = "--settings"
	flagModel           = "--model"
	flagEffort          = "--effort"
)

var launchFlags = []string{flagAgentID, flagAgentType, flagParentSessionID, flagSettings, flagModel, flagEffort}

// LaunchMain is `cc-fleet __teammate-launch`; args = os.Args[2:]. On a
// successful route or passthrough it does not return (syscall.Exec).
func LaunchMain(args []string) int {
	return launchMain(args, os.Stdout, os.Stderr)
}

// LaunchError is a launcher failure, printed as one failure line.
type LaunchError struct{ Code, AgentID, Msg, Suggestion string }

func (e *LaunchError) Error() string { return e.Line() }

// Line = FormatFailureLine(e.Code, e.AgentID, e.Msg, e.Suggestion).
func (e *LaunchError) Line() string {
	return FormatFailureLine(e.Code, e.AgentID, e.Msg, e.Suggestion)
}

// runLauncher is the unix launcher: shim handshake, then passthrough or
// provider routing. Any failure is one stderr line and exit 1, with no exec.
func runLauncher(args []string, stdout, stderr io.Writer) int {
	if len(args) == 2 && args[0] == "--shim-protocol" {
		if args[1] == strconv.Itoa(ShimProtocol) {
			fmt.Fprintln(stdout, ShimProtocol)
			return 0
		}
		return 3
	}
	if err := launch(args); err != nil {
		fmt.Fprintln(stderr, err.Line())
		return 1
	}
	return 0 // only reached when execFn is a test stub
}

func launch(args []string) *LaunchError {
	vals, _ := scanLaunchArgv(args, nil)
	id := failureAgentID(vals[flagAgentID])

	t, ours, err := ParseAgentType(vals[flagAgentType])
	if !ours {
		// Native teammate: argv and env go through untouched, nothing is written.
		bin, lerr := leadBinary(id, vals[flagParentSessionID])
		if lerr != nil {
			return lerr
		}
		return execLead(id, bin, args, os.Environ())
	}
	if err != nil {
		return &LaunchError{CodeBadArgs, id, err.Error(), "use ccf-<provider>[.strong|.fast] for a provider teammate, or a native agent type for Claude"}
	}
	if err := validateAgentID(vals[flagAgentID]); err != nil {
		return &LaunchError{CodeBadArgs, id, err.Error(), "start provider teammates with the Agent tool: Agent({name, subagent_type, prompt})"}
	}

	cfg, err := config.Load()
	if err != nil {
		return &LaunchError{CodeConfigLoadFailed, id, err.Error(), "fix providers.toml, then run `cc-fleet doctor`"}
	}
	v := cfg.Providers[t.Provider]
	if v == nil {
		return &LaunchError{CodeUnknownProvider, id, fmt.Sprintf("provider %q is not configured", t.Provider), "run `cc-fleet list`, or add it with `cc-fleet add`"}
	}
	if !v.Enabled {
		return &LaunchError{CodeProviderDisabled, id, fmt.Sprintf("provider %q is disabled", t.Provider), fmt.Sprintf("run `cc-fleet edit %s --enable`", t.Provider)}
	}
	slot := ""
	if t.Slot != SlotDefault {
		slot = string(t.Slot)
	}
	model := v.ResolveModel(slot)

	bin, lerr := leadBinary(id, vals[flagParentSessionID])
	if lerr != nil {
		return lerr
	}
	if v.DaemonBacked() {
		if err := ensureDaemonFn(v); err != nil {
			return &LaunchError{CodeCodexProxyUnavailable, id, fmt.Sprintf("the local proxy for %s could not start: %v", t.Provider, err), "run `cc-fleet codex-proxy status`, then retry"}
		}
	}
	prof, err := writeProfileFn(v)
	if err != nil {
		return &LaunchError{CodeProfileWriteFailed, id, fmt.Sprintf("could not write the profile for %s: %v", t.Provider, err), "check permissions on ~/.claude/profiles, then run `cc-fleet repair`"}
	}

	// The type is routed here (and its definition body must never replace the
	// system prompt), --settings is single-valued so the profile must win, and
	// the model comes only from the type. A provider effort overrides the lead's.
	drop := map[string]bool{flagAgentType: true, flagSettings: true, flagModel: true}
	if v.Effort != "" {
		drop[flagEffort] = true
	}
	_, argv := scanLaunchArgv(args, drop)
	argv = append(argv, flagSettings, prof, flagModel, model)
	return execLead(id, bin, argv, childenv.CleanForTeammate(os.Environ()))
}

// scanLaunchArgv walks argv once. vals holds the last value of each launcher
// flag; kept is argv with the flags in drop removed (both spellings, value
// included). Every other token is kept in place.
func scanLaunchArgv(args []string, drop map[string]bool) (vals map[string]string, kept []string) {
	vals = map[string]string{}
	kept = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		name, val, inline := matchLaunchFlag(args[i])
		if name == "" {
			kept = append(kept, args[i])
			continue
		}
		tokens := args[i : i+1]
		if !inline && i+1 < len(args) {
			val = args[i+1]
			tokens = args[i : i+2]
			i++
		}
		vals[name] = val
		if !drop[name] {
			kept = append(kept, tokens...)
		}
	}
	return vals, kept
}

// matchLaunchFlag reports which launcher flag tok is: `--x` (value in the next
// token) or `--x=v` (inline). name is "" for any other token.
func matchLaunchFlag(tok string) (name, val string, inline bool) {
	for _, f := range launchFlags {
		if tok == f {
			return f, "", false
		}
		if v, ok := strings.CutPrefix(tok, f+"="); ok {
			return f, v, true
		}
	}
	return "", "", false
}

// validateAgentID checks the `<name>@<team>` shape CC passes as --agent-id.
func validateAgentID(s string) error {
	name, team, ok := strings.Cut(s, "@")
	if !ok {
		return fmt.Errorf("--agent-id %q is not <name>@<team>", s)
	}
	if err := ids.ValidateMemberName(name); err != nil {
		return err
	}
	return ids.ValidateTeamName(team)
}

// failureAgentID is the agent id field of a failure line; "-" when the id is
// missing or would break ParseFailureLine's grammar.
func failureAgentID(s string) string {
	if s == "" || strings.ContainsAny(s, ": \t\r\n\v\f") {
		return "-"
	}
	return s
}

// leadBinary resolves the claude to exec and refuses cc-fleet itself or its
// shim, which would loop.
func leadBinary(id, parentSessionID string) (string, *LaunchError) {
	bin, err := resolveLeadBinaryFn(parentSessionID)
	if err != nil {
		return "", &LaunchError{CodeClaudeNotFound, id, err.Error(), "install Claude Code or put `claude` on PATH, then retry"}
	}
	if isLauncherItself(bin) {
		return "", &LaunchError{CodeClaudeNotFound, id, fmt.Sprintf("%s is cc-fleet's own launcher, not claude; refusing to exec a loop", bin), "put the real claude first on PATH, then retry"}
	}
	return bin, nil
}

// resolveLeadBinary picks the claude matching the lead's version: the live
// session named by --parent-session-id reports it, claudebin.ForSession maps
// it to versions/<v>. Without that session (or that file) it is Resolve's.
func resolveLeadBinary(parentSessionID string) (string, error) {
	// claudebin runs `<candidate> --version` when it falls back to the PATH
	// lookup; a candidate that is cc-fleet itself would recurse into the launcher.
	if p, ok := ccver.Located(); ok && isLauncherItself(p) {
		return "", fmt.Errorf("%s is cc-fleet's own launcher, not claude; refusing to exec a loop", p)
	}
	version := ""
	if s, ok := leadsession.BySessionID(parentSessionID); ok {
		version = s.Version
	}
	bin, err := claudebin.ForSession(version)
	if err != nil {
		return "", fmt.Errorf("cannot find the claude binary for this session (version %q): %w", version, err)
	}
	return bin, nil
}

// isLauncherItself reports whether bin is the same file as the running
// cc-fleet or the shim (os.Stat follows symlinks).
func isLauncherItself(bin string) bool {
	fi, err := os.Stat(bin)
	if err != nil {
		return false
	}
	var self []string
	if exe, err := os.Executable(); err == nil {
		self = append(self, exe)
	}
	if shim, err := ShimPath(); err == nil {
		self = append(self, shim)
	}
	for _, p := range self {
		if si, err := os.Stat(p); err == nil && os.SameFile(fi, si) {
			return true
		}
	}
	return false
}

func execLead(id, bin string, argv, env []string) *LaunchError {
	if err := execFn(bin, append([]string{bin}, argv...), env); err != nil {
		return &LaunchError{CodeInternal, id, fmt.Sprintf("exec %s: %v", bin, err), "run `cc-fleet doctor`"}
	}
	return nil
}
