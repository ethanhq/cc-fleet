package subagent

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/ethanhq/cc-fleet/internal/ccver"
	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/fingerprint"
)

// SlimVersionFloor is the lowest claude version whose source carries every flag
// the slim profiles emit (--system-prompt-file / --tools / --thinking disabled /
// --strict-mcp-config). Below it the version gate runs `full` instead.
const SlimVersionFloor = "2.1.88"

// Prompt profiles. "" and ProfileFull mean today's full `claude -p` session
// (byte-identical argv); the two slim profiles mirror native Claude Code agent
// classes — ProfileSlim the generic subagent (keeps CLAUDE.md + gitStatus),
// ProfileSlimRO the read-only Explore/Plan agent (drops both).
const (
	ProfileFull   = "full"
	ProfileSlim   = "slim"
	ProfileSlimRO = "slim-ro"
)

// ValidateProfile accepts "" and "full" (both full), "slim", "slim-ro";
// anything else is an error.
func ValidateProfile(p string) error {
	switch p {
	case "", ProfileFull, ProfileSlim, ProfileSlimRO:
		return nil
	default:
		return fmt.Errorf("unknown prompt profile %q (want full|slim|slim-ro)", p)
	}
}

// resolveBinaryPathVersion resolves (path, version) for the exact binary that
// will run. A var so tests can supply a fake version without a real claude.
var resolveBinaryPathVersion = fingerprint.ResolveBinaryPathVersion

// ResolveEffectiveProfile maps a REQUESTED profile to the one that will actually
// run, applying the version gate. full/"" pass through unchanged. A slim profile
// is kept only when the resolved claude version is at or above SlimVersionFloor;
// a below-floor / unknown version — or any error resolving the binary — fails
// OPEN to "full" with a human downgrade reason (never silent, never failing the
// leaf for an optimization).
//
// fp is the fingerprint already loaded by the caller (Run loads it once; the
// workflow engine loads it the same way) — the version is resolved against THAT
// recipe's binary, never a re-loaded one, so the gate can't read a different
// executable than the one Run resolved. Version resolution is process-cached per
// resolved path in the fingerprint resolver.
func ResolveEffectiveProfile(requested string, fp *fingerprint.Fingerprint) (effective string, downgrade string) {
	if requested == "" || requested == ProfileFull {
		return requested, ""
	}
	_, version, err := resolveBinaryPathVersion(fp)
	if err != nil {
		return ProfileFull, fmt.Sprintf("slim disabled: resolve claude binary: %v", err)
	}
	if !ccver.AtLeast(version, SlimVersionFloor) {
		shown := version
		if shown == "" {
			shown = "unknown"
		}
		return ProfileFull, fmt.Sprintf("slim disabled: claude version %s below floor %s", shown, SlimVersionFloor)
	}
	return requested, ""
}

// validateSlimArgs front-loads the profile + slim-refinement validation: the
// profile enum, the slim-only refinements (tools / skills-off / mcp) rejected
// when combined with the full profile, and CanonicalizeTools on an explicit
// tool set. It returns a SUBAGENT_BAD_ARGS Result (no exec) on the first
// violation, else nil. Mirrors the CLI's front-loaded check so the engine and
// bare-CLI paths reject identically.
func validateSlimArgs(req Request) *Result {
	if err := ValidateProfile(req.PromptProfile); err != nil {
		r := fail(ErrCodeBadArgs, err.Error(), req.Provider, "")
		return &r
	}
	isFull := req.PromptProfile == "" || req.PromptProfile == ProfileFull
	if isFull && (len(req.Tools) > 0 || req.NoSkills || req.MCP) {
		r := fail(ErrCodeBadArgs,
			"--tools / --skills / --mcp are slim-only; they require --profile slim or slim-ro",
			req.Provider, "")
		return &r
	}
	if len(req.Tools) > 0 {
		if _, err := CanonicalizeTools(req.Tools); err != nil {
			r := fail(ErrCodeBadArgs, fmt.Sprintf("invalid --tools: %v", err), req.Provider, "")
			return &r
		}
	}
	if err := ValidateToolsSkills(req.Tools, req.NoSkills); err != nil {
		r := fail(ErrCodeBadArgs, err.Error(), req.Provider, "")
		return &r
	}
	return nil
}

// ValidateToolsSkills rejects the contradictory combination of an explicit tool set that
// names "Skill" together with skills disabled (NoSkills): the caller both requests and
// forbids the Skill tool. Shared so the bare-CLI, Run, and workflow paths reject it
// identically. nil when the inputs are consistent.
func ValidateToolsSkills(tools []string, noSkills bool) error {
	if !noSkills {
		return nil
	}
	for _, n := range tools {
		if n == "Skill" {
			return errors.New(`"Skill" in tools is contradictory with skills disabled`)
		}
	}
	return nil
}

// buildSlimArgv renders + writes the per-job slim prompt sidecar and returns the
// slim-profile argv additions. For a full/"" effective profile it returns the
// zero slimArgv (no slim flags), keeping the full argv byte-identical. A slim
// profile needs a job id for the <jobID>.slimprompt sidecar; an empty id (the
// jobs dir was unavailable) is an error since --system-prompt-file then has no
// target. The tool set is the canonicalized explicit Tools or the profile
// default; CanonicalizeTools was already validated front-loaded.
func buildSlimArgv(effective, jobID string, req Request, model string) (slimArgv, error) {
	if effective == "" || effective == ProfileFull {
		return slimArgv{}, nil
	}
	if jobID == "" {
		return slimArgv{}, errors.New("slim profile: jobs dir unavailable for the prompt sidecar")
	}
	names := req.Tools
	if len(names) == 0 {
		names = DefaultSlimTools(effective, req.NoSkills)
	}
	tools, err := CanonicalizeTools(names)
	if err != nil {
		return slimArgv{}, fmt.Errorf("slim tools: %w", err)
	}
	prompt, err := RenderSlimPrompt(effective, req.WorkingDir, model)
	if err != nil {
		return slimArgv{}, err
	}
	path, err := slimPromptPath(jobID)
	if err != nil {
		return slimArgv{}, fmt.Errorf("slim prompt path: %w", err)
	}
	if err := fileutil.AtomicWrite(path, []byte(prompt), 0o600); err != nil {
		return slimArgv{}, fmt.Errorf("write slim prompt: %w", err)
	}
	return slimArgv{promptFile: path, tools: tools}, nil
}

// canonicalTools is the floor-version (2.1.88) set of stable, always-present
// built-in tool names — the names visible in a 2.1.x request. Env-gated tools
// (Config/Tungsten/LSP/EnterWorktree/cron/…) and the legacy Task alias are
// excluded: --tools validates against exactly the names a slim worker can
// reliably get. Sourced from getAllBaseTools (tools.ts) and the per-tool name
// constants.
var canonicalTools = map[string]struct{}{
	"Agent":           {},
	"AskUserQuestion": {},
	"Bash":            {},
	"Edit":            {},
	"Glob":            {},
	"Grep":            {},
	"NotebookEdit":    {},
	"Read":            {},
	"Skill":           {},
	"TodoWrite":       {},
	"WebFetch":        {},
	"WebSearch":       {},
	"Write":           {},
}

// DefaultSlimTools returns a profile's default tool set: slim is read-write
// (Bash,Read,Edit,Write,Grep,Glob), slim-ro is read-only (Bash,Read,Grep,Glob);
// each adds Skill unless noSkills. Bash is present in slim-ro exactly as in
// native Explore — read-only is a prompt-level contract, not sandbox
// enforcement.
func DefaultSlimTools(profile string, noSkills bool) []string {
	var tools []string
	switch profile {
	case ProfileSlimRO:
		tools = []string{"Bash", "Read", "Grep", "Glob"}
	default:
		tools = []string{"Bash", "Read", "Edit", "Write", "Grep", "Glob"}
	}
	if !noSkills {
		tools = append(tools, "Skill")
	}
	return tools
}

// CanonicalizeTools validates every name against the canonical floor-version
// built-in set and returns the set deduped + sorted, so caller order never
// changes the result (the argv join and the journal key both depend on this).
// Unknown, duplicate, or empty entries are rejected.
func CanonicalizeTools(names []string) ([]string, error) {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" {
			return nil, errors.New("empty tool name")
		}
		if _, ok := canonicalTools[n]; !ok {
			return nil, fmt.Errorf("unknown tool %q", n)
		}
		if _, dup := seen[n]; dup {
			return nil, fmt.Errorf("duplicate tool %q", n)
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

//go:embed slimprompt.txt.tmpl
var slimPromptTemplate string

var slimTmpl = template.Must(template.New("slim").Parse(slimPromptTemplate))

// slimPromptData feeds the embedded native-mirror template.
type slimPromptData struct {
	ReadOnly   bool // slim-ro: render the read-only research paragraph
	WorkingDir string
	IsGitRepo  bool
	Platform   string
	Shell      string
	OSVersion  string
	Date       string
	Model      string
	GitStatus  string // slim only, inside a git repo; already truncated
}

// RenderSlimPrompt renders the native-mirror system prompt for a slim profile:
// identity + agent prompt, the Notes block, the <env> block, and the model
// line. slim-ro adds a read-only research paragraph; slim (only) appends a
// gitStatus snapshot when workingDir is inside a git repo. profile must be
// ProfileSlim or ProfileSlimRO.
func RenderSlimPrompt(profile, workingDir, model string) (string, error) {
	if profile != ProfileSlim && profile != ProfileSlimRO {
		return "", fmt.Errorf("RenderSlimPrompt: profile %q is not a slim profile", profile)
	}
	wd := workingDir
	if wd == "" {
		if cwd, err := os.Getwd(); err == nil {
			wd = cwd
		}
	}
	isGit := isGitRepo(wd)
	data := slimPromptData{
		ReadOnly:   profile == ProfileSlimRO,
		WorkingDir: wd,
		IsGitRepo:  isGit,
		Platform:   runtime.GOOS,
		Shell:      shellName(),
		OSVersion:  osVersion(),
		Date:       time.Now().Format("2006-01-02"),
		Model:      model,
	}
	if profile == ProfileSlim && isGit {
		data.GitStatus = gitStatus(wd)
	}
	var buf bytes.Buffer
	if err := slimTmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render slim prompt: %w", err)
	}
	return buf.String(), nil
}

// shellName is the basename of $SHELL for the <env> Shell line, "unknown" when $SHELL
// is unset (the common headless/Windows case).
func shellName() string {
	sh := os.Getenv("SHELL")
	if sh == "" {
		return "unknown"
	}
	return filepath.Base(sh)
}

// osVersion mirrors native CC's `uname -sr` OS Version line via a bounded exec,
// falling back to GOOS on any error (Windows has no uname).
func osVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uname", "-sr")
	cmd.Env = childenv.Clean(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return runtime.GOOS
	}
	return strings.TrimSpace(string(out))
}

// isGitRepo reports whether dir is inside a git working tree, bounded and
// best-effort (false on any error).
func isGitRepo(dir string) bool {
	out, err := runGitBounded(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

const maxGitStatusChars = 2000

// gitStatus mirrors native CC's gitStatus snapshot: branch, `git status
// --short`, and the last 5 commits, the whole block truncated at 2000 chars.
// Silently empty on any git error.
func gitStatus(dir string) string {
	branch, err := runGitBounded(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	status, err := runGitBounded(dir, "status", "--short")
	if err != nil {
		return ""
	}
	log, err := runGitBounded(dir, "log", "--oneline", "-n", "5")
	if err != nil {
		return ""
	}
	statusBody := strings.TrimSpace(status)
	if statusBody == "" {
		statusBody = "(clean)"
	}
	block := strings.Join([]string{
		"This is the git status at the start of the conversation. Note that this status is a snapshot in time, and will not update during the conversation.",
		"Current branch: " + strings.TrimSpace(branch),
		"Status:\n" + statusBody,
		"Recent commits:\n" + strings.TrimSpace(log),
	}, "\n\n")
	if len(block) > maxGitStatusChars {
		block = block[:maxGitStatusChars]
	}
	return block
}

// runGitBounded runs a git command in dir with a 2s deadline and a cred-scrubbed
// env, returning combined output.
func runGitBounded(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = childenv.Clean(os.Environ())
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}
