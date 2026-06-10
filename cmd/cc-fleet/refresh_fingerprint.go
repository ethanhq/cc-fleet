package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethanhq/cc-fleet/internal/fingerprint"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// refreshFingerprintError is the JSON error envelope written to stdout when
// --json is set. The shape is deliberately tiny so skill code can dispatch on
// `error_code` without parsing prose.
type refreshFingerprintError struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error"`
	ErrorCode string `json:"error_code,omitempty"`
}

// refreshFingerprintSuccess is the JSON success envelope.
type refreshFingerprintSuccess struct {
	OK              bool      `json:"ok"`
	FingerprintPath string    `json:"fingerprint_path"`
	CCVersion       string    `json:"cc_version"`
	CapturedAt      time.Time `json:"captured_at"`
}

// Error code constants — keep them stable; extending the list without updating
// the skill breaks JSON consumers.
const (
	codeProbeNotFound = "PROBE_NOT_FOUND"
	codeProbeTeamReq  = "PROBE_TEAM_REQUIRED"
	codeCaptureFailed = "CAPTURE_FAILED"
	codeSaveFailed    = "SAVE_FAILED"
)

// probeAgentType is the --agent-type value the probe teammate always carries;
// the lead session never does, so it anchors the probe match.
const probeAgentType = "general-purpose"

// Seams: process introspection + capture/save are package vars so the probe-
// selection / re-validation logic is unit-testable without a live process,
// provider, or network.
var (
	listProcesses   = procintrospect.ProcessTable
	procStartToken  = procintrospect.ProcStart
	readArgv        = procintrospect.Cmdline
	captureFromPid  = fingerprint.CaptureFromPid
	saveFingerprint = fingerprint.Save
)

func newRefreshFingerprintCmd() *cobra.Command {
	var (
		probeTeam string
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "refresh-fingerprint",
		Short: "Re-probe the Claude Code spawn template via a live probe agent",
		Long: `Re-snapshot the env vars + flag template that Claude Code uses to spawn
native Agent teammates.

The probe itself runs as a real teammate spawned by the skill (via the
native Agent tool) — this command only locates that teammate's process and
reads its spawn template, then writes it to ~/.config/cc-fleet/fingerprint.json.
How the template is read is platform-specific:

  - Linux: reads /proc/<pid>/{cmdline,environ} directly.
  - macOS: no /proc, so the argv is read via "ps" and the two known env
    constants (CLAUDECODE, CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS) are
    synthesized rather than read from the probe's environ.

The probe is matched on exact argv tokens (--team-name <team> and
--agent-type general-purpose), never a loose pattern, and its identity is
re-validated just before the fingerprint is saved.

Use --probe-team to select which team's probe to snapshot. The skill's
self-heal flow names its probe team "_ccf-probe-<uuid>"; pass that here.`,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRefreshFingerprint(probeTeam, asJSON)
		},
	}

	cmd.Flags().StringVar(&probeTeam, "probe-team", "",
		"Team name of the probe teammate to snapshot (required, e.g. _ccf-probe-<uuid>)")
	cmd.Flags().BoolVar(&asJSON, "json", false,
		"Emit machine-readable JSON on stdout (for skill consumption)")

	return cmd
}

// runRefreshFingerprint does the work: validate args, locate the probe PID,
// capture its spawn template, RE-VALIDATE the probe's identity, then save the
// fingerprint. Output shape is controlled by asJSON.
//
// On error we always write a single JSON object (in --json mode) or a
// human-readable line (otherwise) and return a non-nil error so cobra exits 1
// — but we suppress cobra's default error echo so JSON consumers see exactly
// one envelope.
func runRefreshFingerprint(probeTeam string, asJSON bool) error {
	if probeTeam == "" {
		return reportErr(asJSON, codeProbeTeamReq,
			errors.New("--probe-team is required (e.g. _ccf-probe-<uuid>)"))
	}
	// Typed identity at the CLI boundary: NewTeamID rejects path separators /
	// ".." / whitespace before the team string is used to match a process;
	// combined with the exact-token match below, a name with regex
	// metacharacters (e.g. "a.b") can't widen the selection.
	teamID, err := ids.NewTeamID(probeTeam)
	if err != nil {
		return reportErr(asJSON, codeProbeTeamReq, fmt.Errorf("invalid --probe-team: %w", err))
	}
	team := teamID.String()

	pid, startToken, err := findProbePid(team)
	if err != nil {
		return reportErr(asJSON, codeProbeNotFound, err)
	}

	fp, err := captureFromPid(pid)
	if err != nil {
		return reportErr(asJSON, codeCaptureFailed,
			fmt.Errorf("capture from pid %d: %w", pid, err))
	}

	// Re-validate the probe's identity AFTER capture and BEFORE the global cache
	// write. If the PID exited or was recycled between selection and capture
	// (start token changed, or it no longer carries the exact probe argv), the
	// captured template may belong to an unrelated process — drop it rather than
	// overwrite the global spawn template.
	if !revalidateProbe(pid, startToken, team) {
		return reportErr(asJSON, codeProbeNotFound,
			fmt.Errorf("probe pid %d changed identity between selection and capture; not saving fingerprint", pid))
	}

	if err := saveFingerprint(fp); err != nil {
		return reportErr(asJSON, codeSaveFailed, err)
	}

	path, _ := fingerprint.Path() // already succeeded inside Save; ignore err
	return reportOK(asJSON, path, fp)
}

// findProbePid enumerates the process table and returns the PID + process start
// token of the SINGLE probe teammate for team: a claude process whose argv
// carries the exact tokens `--team-name <team>` AND `--agent-type
// general-purpose`. Matching is on exact argv tokens (procintrospect, not a
// `pgrep -f` regex substring) so a team name with regex metacharacters can't
// widen the match. If zero OR more than one process matches, it refuses rather
// than capture a guess onto the global template — no first-PID-wins. The start
// token binds the selection so revalidateProbe can detect PID reuse.
func findProbePid(team string) (pid int, startToken string, err error) {
	procs, perr := listProcesses()
	if perr != nil {
		return 0, "", fmt.Errorf("enumerate processes for team %q: %w", team, perr)
	}
	var matches []int
	for _, p := range procs {
		if argvIsProbe(p.Argv, team) {
			matches = append(matches, p.PID)
		}
	}
	switch len(matches) {
	case 0:
		return 0, "", fmt.Errorf("no probe process found for team %q", team)
	case 1:
		pid = matches[0]
	default:
		// Ambiguous: the skill spawns exactly one probe per team; >1 means the
		// wrong team name or a leaked probe. Refuse rather than guess.
		return 0, "", fmt.Errorf("multiple (%d) probe processes match team %q; refusing to guess which to capture", len(matches), team)
	}
	// Best-effort: an empty start token (platform can't expose it) still allows
	// capture; revalidateProbe then relies on the argv re-check alone.
	startToken, _ = procStartToken(pid)
	return pid, startToken, nil
}

// revalidateProbe reports whether pid STILL names the selected probe: the same
// start token (no PID reuse) AND still the exact probe argv. A best-effort empty
// wantStart (the platform couldn't expose a start time at selection) falls back
// to the argv re-check alone.
func revalidateProbe(pid int, wantStart, team string) bool {
	if wantStart != "" {
		gotStart, ok := procStartToken(pid)
		if !ok || gotStart != wantStart {
			return false
		}
	}
	argv, err := readArgv(pid)
	if err != nil {
		return false
	}
	return argvIsProbe(argv, team)
}

// argvIsProbe reports whether argv is the Agent probe for team: a claude
// executable token (a "/claude/" path segment — versions/<hash> basenames
// aren't "claude" — or a basename containing "claude") AND the exact token pair
// `--team-name <team>` AND `--agent-type general-purpose`. Exact-token matching
// (not a regex substring); team is already validated whitespace-free so darwin's
// space-split argv matches it exactly too.
func argvIsProbe(argv []string, team string) bool {
	var isClaude, hasTeam, hasAgentType bool
	for i, a := range argv {
		if a == "" {
			continue
		}
		if !isClaude && (strings.Contains(a, "/claude/") || strings.Contains(filepath.Base(a), "claude")) {
			isClaude = true
		}
		if a == "--team-name" && i+1 < len(argv) && argv[i+1] == team {
			hasTeam = true
		}
		if a == "--agent-type" && i+1 < len(argv) && argv[i+1] == probeAgentType {
			hasAgentType = true
		}
	}
	return isClaude && hasTeam && hasAgentType
}

// reportOK emits the success envelope and returns nil so cobra exits 0.
func reportOK(asJSON bool, path string, fp *fingerprint.Fingerprint) error {
	if asJSON {
		out := refreshFingerprintSuccess{
			OK:              true,
			FingerprintPath: path,
			CCVersion:       fp.CCVersion,
			CapturedAt:      fp.CapturedAt,
		}
		data, err := json.Marshal(out)
		if err != nil {
			// MarshalIndent of a fixed struct should never fail — but if it
			// does, surface it so we don't silently emit garbage.
			fmt.Fprintln(os.Stderr, "refresh-fingerprint: marshal:", err)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		fmt.Printf("captured fingerprint for cc %s at %s\n", fp.CCVersion, path)
	}
	return nil
}

// reportErr emits the failure envelope and returns a non-nil error so cobra
// exits 1. We deliberately format the JSON ourselves rather than letting cobra
// echo its own line — JSON consumers expect exactly one envelope.
func reportErr(asJSON bool, code string, err error) error {
	if asJSON {
		out := refreshFingerprintError{
			OK:        false,
			Error:     err.Error(),
			ErrorCode: code,
		}
		data, mErr := json.Marshal(out)
		if mErr != nil {
			fmt.Fprintln(os.Stderr, "refresh-fingerprint: marshal:", mErr)
			os.Exit(1)
		}
		fmt.Println(string(data))
	} else {
		fmt.Fprintln(os.Stderr, "refresh-fingerprint:", err)
	}
	// Return the original error so main()'s exit code is 1; SilenceErrors is
	// set on the command so cobra won't print it again.
	return err
}
