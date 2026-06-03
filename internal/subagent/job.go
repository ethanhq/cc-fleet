package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// jobsDirName holds background-job files under ConfigDir. Per job_id there are
// up to: <id>.json (meta), <id>.out, <id>.err, <id>.prompt (when --prompt-file),
// and <id>.result.json (cached terminal Result).
const jobsDirName = "subagent-jobs"

// defaultGCAge is the cutoff subagent-gc uses when --older-than is unset.
const defaultGCAge = 24 * time.Hour

// maxPromptBytes bounds a materialized --prompt-file / piped stdin. A task prompt
// is far smaller; claude's own context window is the real ceiling. The cap only
// stops an unbounded caller-supplied reader from OOMing the launch. Package var
// so tests can shrink it.
var maxPromptBytes = 10 << 20 // 10 MiB

// jobMeta is the on-disk record written at --background launch (and a lighter
// "running" record for a sync run, so the board can see it). It carries no
// secret (prompt/answer are intentionally NOT persisted here) — just enough to
// poll the process and re-classify its captured stdout later.
type jobMeta struct {
	JobID         string `json:"job_id"`
	PID           int    `json:"pid"`
	PGID          int    `json:"pgid"`
	Vendor        string `json:"vendor"`
	Model         string `json:"model"`
	StartedAt     string `json:"started_at"`
	Status        string `json:"status"`
	Resume        string `json:"resume,omitempty"`
	OutputFormat  string `json:"output_format,omitempty"`
	JSON          bool   `json:"json"`
	LeadSessionID string `json:"lead_session_id,omitempty"`
	// SettingsPath is the claude `--settings <profile>` for a BACKGROUND job: the
	// per-vendor profile path, unique enough to bind meta.PID to its claude child
	// so processAlive can reject a recycled pid. A sync job leaves this empty —
	// its PID is the cc-fleet process, not a claude child, so the reuse guard
	// must NOT apply (processAlive degrades to a bare kill(0)).
	SettingsPath string `json:"settings_path,omitempty"`
}

func jobsDir() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, jobsDirName), nil
}

// writeMetaFn is a test seam: tests force a writeMeta failure AFTER cmd.Start
// to verify the cleanup path kills the process group and removes the orphan
// .out/.err files.
var writeMetaFn = writeMeta

// materializePromptFn is a test seam: tests swap the helper with a stub that
// leaves a partial dst file behind + returns an error, so the caller's
// `_ = os.Remove(pf)` in the materialize-error branch is testably load-bearing
// on its own (independent of the helper's defer cleanup). In production this is
// just materializePromptReader.
var materializePromptFn = materializePromptReader

// launchBackground starts a detached claude child whose stdout/stderr go to job
// files, writes the job meta, and returns immediately with a job handle. The
// child runs with its OWN process group and NO deadline so it survives the
// parent cc-fleet exiting (poll it with StatusFor / subagent-status).
//
// Background runs always use claude's `--output-format json` so StatusFor can
// rely on the envelope rather than a placeholder exit code, making text-mode
// background failures classify correctly instead of false-reporting `done`.
//
// Any failure between cmd.Start and the final Release triggers a process-group
// SIGTERM (200ms grace) → SIGKILL → Wait → file cleanup so we never leak a
// detached vendor child + orphan .out/.err files.
func launchBackground(req Request, binaryPath, profilePath, model string) Result {
	dir, err := jobsDir()
	if err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("resolve jobs dir: %v", err), req.Vendor, "")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("mkdir jobs dir: %v", err), req.Vendor, "")
	}

	// Force inner JSON so StatusFor classifies via the envelope, not exitCode=0.
	// Outer JSON/text formatting is unaffected (it only changes how
	// reportSubagent prints the final Result).
	innerReq := req
	innerReq.OutputFormat = "json"

	jobID := uuid.NewString()
	outPath := filepath.Join(dir, jobID+".out")
	errPath := filepath.Join(dir, jobID+".err")

	outF, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("create job stdout: %v", err), req.Vendor, "")
	}
	defer outF.Close()
	errF, err := os.OpenFile(errPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.Remove(outPath)
		return fail(ErrCodeFailed, fmt.Sprintf("create job stderr: %v", err), req.Vendor, "")
	}
	defer errF.Close()

	argv := buildArgv(binaryPath, profilePath, model, innerReq)
	// Fresh exec.Command (no context) → no deadline; child outlives parent.
	cmd := exec.Command(binaryPath)
	cmd.Args = argv
	cmd.Env = childenv.Clean(os.Environ())
	cmd.Stdout = outF
	cmd.Stderr = errF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// stdin: a detached child can't keep a parent copy-goroutine alive, so hand
	// it a real inherited fd. An *os.File (the common --prompt-file case) is
	// inherited directly; any other reader is materialized to a job file first.
	//
	// When the reader is NOT an *os.File the materialization must FAIL BEFORE
	// cmd.Start — otherwise a read error would silently hand a partial prompt to
	// claude. Sync (subagent.Run) is unaffected: it inherits stdin directly and
	// never reaches this path.
	if req.PromptReader != nil {
		if f, ok := req.PromptReader.(*os.File); ok {
			cmd.Stdin = f
		} else {
			pf := filepath.Join(dir, jobID+".prompt")
			f, merr := materializePromptFn(req.PromptReader, pf)
			if merr != nil {
				// The helper already removes dst (pf) on its own error path; we
				// ALSO Remove here so "no orphan .prompt after a materialize
				// failure" is re-asserted at the call site (defense-in-depth).
				// All three artifacts are best-effort.
				_ = os.Remove(outPath)
				_ = os.Remove(errPath)
				_ = os.Remove(pf)
				return fail(ErrCodeFailed,
					fmt.Sprintf("materialize prompt: %v", merr), req.Vendor, "")
			}
			defer f.Close()
			cmd.Stdin = f
		}
	}

	if err := cmd.Start(); err != nil {
		_ = os.Remove(outPath)
		_ = os.Remove(errPath)
		_ = os.Remove(filepath.Join(dir, jobID+".prompt"))
		return fail(ErrCodeFailed, fmt.Sprintf("start background subagent: %v", err), req.Vendor, "")
	}
	pid := cmd.Process.Pid

	meta := jobMeta{
		JobID:     jobID,
		PID:       pid,
		PGID:      pid, // Setpgid → the group id equals the leader pid
		Vendor:    req.Vendor,
		Model:     model,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Status:    "running",
		Resume:    req.Resume,
		// Persist the outer (user-facing) format flags so subagent-status can
		// still render the operator's preferred shape (text vs JSON).
		// OutputFormat stays as the caller asked; JSON is force-set true below
		// regardless of the caller, because the inner argv is ALWAYS
		// --output-format json for background launches, so StatusFor must
		// classify stdout as an envelope. Without it, a text-mode background job
		// whose child wrote a JSON error envelope would be blessed as
		// status:"done" by the text-mode fallback.
		OutputFormat:  req.OutputFormat,
		JSON:          true,
		LeadSessionID: req.LeadSessionID,
		SettingsPath:  profilePath, // binds this pid to its claude child (reuse guard)
	}
	if err := writeMetaFn(dir, meta); err != nil {
		// meta write failed AFTER cmd.Start. Without cleanup the detached vendor
		// child + .out / .err would orphan. Process-group kill (SIGTERM → 200ms
		// grace → SIGKILL) reaps the child (and any claude-forked grandchild);
		// Wait() reclaims the zombie before Release. Then nuke the captured files.
		killProcessGroup(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
		_ = os.Remove(outPath)
		_ = os.Remove(errPath)
		_ = os.Remove(filepath.Join(dir, jobID+".prompt"))
		return fail(ErrCodeFailed, fmt.Sprintf("write job meta: %v", err), req.Vendor, "")
	}

	// Detach: stop tracking the child so the parent can exit cleanly.
	_ = cmd.Process.Release()

	return Result{
		OK:            true,
		JobID:         jobID,
		Status:        "running",
		OutputFile:    outPath,
		PID:           pid,
		Vendor:        req.Vendor,
		Model:         model,
		StartedAt:     meta.StartedAt,
		LeadSessionID: meta.LeadSessionID,
	}
}

// materializePromptReader copies r into a 0o600 file at dst and returns an
// *os.File positioned at offset 0 ready to be inherited as the child's stdin.
// Errors MUST be returned and surfaced to the caller before cmd.Start so the
// child never receives a partial prompt.
//
// On any failure dst is removed best-effort via a deferred named-return
// cleanup, so a truncated/partial file can't survive looking like a finished
// job's .prompt. r is NOT closed; the caller owns its lifetime.
//
// The nil-reader path returns BEFORE any filesystem operations, so the deferred
// Remove cannot delete an unrelated pre-existing file at dst.
func materializePromptReader(r io.Reader, dst string) (f *os.File, err error) {
	if r == nil {
		return nil, nil
	}
	// Clean up dst on every error path uniformly. Runs only when err != nil so
	// the happy-path caller can use the returned file without losing its data.
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	// Bounded read: a --prompt-file / piped stdin is caller-supplied and otherwise
	// unbounded. LimitReader+1 distinguishes "exactly the cap" from "over"; an
	// overflow fails here, BEFORE cmd.Start, so the child never gets a partial prompt.
	data, err := io.ReadAll(io.LimitReader(r, int64(maxPromptBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read prompt: %w", err)
	}
	if len(data) > maxPromptBytes {
		return nil, fmt.Errorf("prompt exceeds %d bytes", maxPromptBytes)
	}
	if err = os.WriteFile(dst, data, 0o600); err != nil {
		return nil, fmt.Errorf("write prompt %s: %w", dst, err)
	}
	f, err = os.Open(dst)
	if err != nil {
		return nil, fmt.Errorf("open prompt %s: %w", dst, err)
	}
	return f, nil
}

// killProcessGroup sends SIGTERM to the process group (-pid → all members of
// pgid pid), waits a short grace, then SIGKILLs survivors. Best-effort: ESRCH
// (already exited) is silently ok. A package var only to allow test injection.
var killProcessGroup = func(pid int) {
	if pid <= 0 {
		return
	}
	// Negative pid → group. The leader has Setpgid:true, so its pid == pgid.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	// Probe + escalate. ESRCH means the group is gone — leave it.
	if err := syscall.Kill(-pid, 0); err == nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// StatusFor reports a background job's status. While the process is alive it
// returns status=running; once dead it classifies the captured stdout with the
// SAME classifier as the sync path, caches the terminal Result to
// <id>.result.json, and returns done/failed.
func StatusFor(jobID string) Result {
	// jobID flows straight into filepath.Join below; validate it before any
	// path is built so a "../" arg can't read outside the jobs dir.
	if err := ids.ValidateJobID(jobID); err != nil {
		return fail(ErrCodeBadArgs, fmt.Sprintf("invalid job id %q", jobID), "",
			"Check the job_id printed by the --background launch")
	}
	dir, err := jobsDir()
	if err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("resolve jobs dir: %v", err), "", "")
	}
	meta, err := readMeta(dir, jobID)
	if err != nil {
		return fail(ErrCodeBadArgs, fmt.Sprintf("unknown job %q", jobID), "",
			"Check the job_id printed by the --background launch")
	}

	// Already finalized? Serve the cached Result.
	resultPath := filepath.Join(dir, jobID+".result.json")
	if data, rerr := os.ReadFile(resultPath); rerr == nil {
		var r Result
		if json.Unmarshal(data, &r) == nil {
			if r.LeadSessionID == "" {
				r.LeadSessionID = meta.LeadSessionID
			}
			return r
		}
	}

	if processAlive(meta.PID, meta.SettingsPath) {
		return Result{
			OK:            true,
			JobID:         jobID,
			Status:        "running",
			Vendor:        meta.Vendor,
			Model:         meta.Model,
			StartedAt:     meta.StartedAt,
			PID:           meta.PID,
			OutputFile:    filepath.Join(dir, jobID+".out"),
			LeadSessionID: meta.LeadSessionID,
		}
	}

	// Dead → classify the captured output. The detached child was Released, so
	// we can't reap its exit code; classification keys on the envelope (json
	// path), not the exit code, so 0 is a safe placeholder.
	stdout, _ := os.ReadFile(filepath.Join(dir, jobID+".out"))
	stderr, _ := os.ReadFile(filepath.Join(dir, jobID+".err"))
	innerJSON := meta.JSON || meta.OutputFormat == "json"
	req := Request{Vendor: meta.Vendor, Model: meta.Model, JSON: meta.JSON, OutputFormat: meta.OutputFormat}
	res := classify(req, meta.Model, stdout, stderr, 0, false, innerJSON)
	res.JobID = jobID
	res.StartedAt = meta.StartedAt
	res.LeadSessionID = meta.LeadSessionID
	if res.OK {
		res.Status = "done"
	} else {
		res.Status = "failed"
	}
	// Cache the terminal result (best-effort; a failed cache just re-classifies).
	if data, merr := json.Marshal(res); merr == nil {
		_ = os.WriteFile(resultPath, data, 0o600)
	}
	return res
}

// ListJobs scans the jobs dir and returns each background job's current Result
// via StatusFor, newest first (by StartedAt). A missing jobs dir yields an empty
// slice and no error (nothing has run yet). Like StatusFor it's read-only with
// respect to team/settings state; the only side effect is StatusFor caching a
// just-finished job's terminal <id>.result.json (benign, idempotent).
func ListJobs() ([]Result, error) {
	dir, err := jobsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subagent: read jobs dir: %w", err)
	}
	var jobs []Result
	for _, e := range entries {
		name := e.Name()
		// Same filter as GC: meta files only, never the cached .result.json.
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		jobID := strings.TrimSuffix(name, ".json")
		jobs = append(jobs, StatusFor(jobID))
	}
	// StartedAt is RFC3339, lexically sortable; descending = newest first.
	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].StartedAt > jobs[j].StartedAt
	})
	return jobs, nil
}

// GC removes the file group of every finished job older than olderThan. A job
// is "finished" when its process is no longer alive (or its terminal result is
// cached). Running jobs are always kept regardless of age.
//
// olderThan semantics: a NEGATIVE duration is treated as "unset" and falls back
// to defaultGCAge; ZERO means "no age limit — remove every finished job"
// (cutoff = now), which is how `subagent-gc --older-than 0s` clears the board's
// done entries. The CLI defaults --older-than to 24h, so an unset invocation
// passes 24h and never hits the zero case by accident.
func GC(olderThan time.Duration) Result {
	if olderThan < 0 {
		olderThan = defaultGCAge
	}
	dir, err := jobsDir()
	if err != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("resolve jobs dir: %v", err), "", "")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{OK: true, Removed: 0} // nothing to GC
		}
		return fail(ErrCodeFailed, fmt.Sprintf("read jobs dir: %v", err), "", "")
	}

	cutoff := time.Now().Add(-olderThan)
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		jobID := strings.TrimSuffix(name, ".json")
		meta, merr := readMeta(dir, jobID)
		if merr != nil {
			continue
		}
		// A cached <id>.result.json is the authoritative terminal signal.
		// processAlive can lie under PID reuse (sync jobs record the cc-fleet
		// PID with empty SettingsPath, so a recycled PID looks alive forever).
		// When the cache exists we KNOW the job is done; honor that first so
		// finished sync jobs get GC'd regardless of PID liveness, and fall back
		// to the liveness check only when the cache is absent.
		resultPath := filepath.Join(dir, jobID+".result.json")
		_, resultErr := os.Stat(resultPath)
		resultCached := resultErr == nil
		if !resultCached && processAlive(meta.PID, meta.SettingsPath) {
			continue // truly running (no result cache + alive process)
		}
		if started, perr := time.Parse(time.RFC3339, meta.StartedAt); perr == nil && started.After(cutoff) {
			continue // finished but too recent
		}
		removeJob(dir, jobID)
		removed++
	}
	return Result{OK: true, Removed: removed}
}

// PurgeJobs is the uninstall-time cleanup of ConfigDir()/subagent-jobs. It is a
// sibling of GC but unconditional on age: it removes the full file group
// (.json/.out/.err/.prompt/.result.json) of every FINISHED job — even when OTHER
// jobs are still running — and keeps only the running ones. So a live background
// subagent's files are never yanked out from under it, while finished jobs'
// (possibly sensitive) .prompt/.result.json are still cleaned up. The now-empty
// jobs dir is removed only when nothing is left running. Returns the
// removed-finished job IDs and the kept-running job IDs (both sorted). A missing
// jobs dir is not an error — nothing has ever run — and returns both empty.
//
// "running" uses the SAME signal as GC: a cached <id>.result.json is the
// authoritative terminal marker (a finished job is never "running" even if its
// pid was recycled); only when it's absent do we fall back to processAlive. An
// unreadable meta can't be polled, so it is treated as finished garbage and removed.
func PurgeJobs() (dir string, removedFinished []string, running []string, err error) {
	dir, err = jobsDir()
	if err != nil {
		return "", nil, nil, err
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return dir, nil, nil, nil // nothing has ever run
		}
		return dir, nil, nil, fmt.Errorf("subagent: read jobs dir: %w", rerr)
	}

	for _, e := range entries {
		name := e.Name()
		// Same filter as GC/ListJobs: meta files only, never the cached .result.json.
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		jobID := strings.TrimSuffix(name, ".json")
		// result-cache-first liveness (mirrors GC): a cached terminal result means
		// done regardless of pid; only without it do we consult processAlive. A
		// meta we can't read can't be polled, so it falls through to removal.
		resultPath := filepath.Join(dir, jobID+".result.json")
		if _, resultErr := os.Stat(resultPath); resultErr != nil {
			if meta, merr := readMeta(dir, jobID); merr == nil && processAlive(meta.PID, meta.SettingsPath) {
				running = append(running, jobID)
				continue // live → keep this job's file group
			}
		}
		// Finished (or dead / unreadable) → remove its full file group now, even
		// if OTHER jobs are still running (partial clean).
		removeJob(dir, jobID)
		removedFinished = append(removedFinished, jobID)
	}

	sort.Strings(removedFinished)
	sort.Strings(running)

	// Drop the (now-empty) dir only when nothing is left running. os.Remove is
	// best-effort: a kept running job — or a stray non-job file — keeps the dir,
	// which is correct.
	if len(running) == 0 {
		_ = os.Remove(dir)
	}
	return dir, removedFinished, running, nil
}

// procRoot is the procfs mount point. A package var so tests can point the
// PID-reuse guard at a fixture tree instead of the live /proc.
var procRoot = "/proc"

// processAlive reports whether pid is alive AND (when settingsPath is known)
// still the claude subagent this job launched. A bare kill(pid,0) only proves
// SOME process holds the pid — after a finished job's pid is recycled, an
// unrelated process would falsely read "running" forever (StatusFor) and never
// GC. So given the job's --settings marker we additionally require the live
// process's cmdline to still look like that claude child. kill(pid,0): nil →
// alive; ESRCH → gone; EPERM → alive but not ours. An empty settingsPath (a sync
// job — its pid is cc-fleet, not a claude child — or a legacy meta) and any
// platform without process introspection degrade to the bare kill(0).
func processAlive(pid int, settingsPath string) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	alive := err == nil || errors.Is(err, syscall.EPERM)
	if !alive {
		return false
	}
	if settingsPath == "" {
		return true // no marker to bind the pid to → trust kill(0)
	}
	// Run the cmdline reuse guard on every platform that can introspect a live
	// process — linux via /proc, darwin via ps (procintrospect). Platforms with
	// neither degrade to the bare kill(0).
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return true
	}
	return cmdlineIsClaudeJob(pid, settingsPath)
}

// cmdlineIsClaudeJob reads pid's argv and reports whether it is still the claude
// subagent for this job: a claude binary (an arg whose path contains "/claude/"
// or whose basename contains "claude" — versions paths have a hash basename, so
// the path segment is the reliable marker) AND this job's --settings
// <profilePath> value (per-vendor, unique enough to bind the pid). Matching
// --settings alone is deliberate: --model is too loose (many jobs share a
// model). If the cmdline can't be read (a just-exited pid / proc race) we trust
// the kill(0) liveness and return true to avoid a flaky false-dead; the
// long-lived recycled-pid footgun always has a readable, non-matching cmdline.
func cmdlineIsClaudeJob(pid int, settingsPath string) bool {
	argv, ok := reuseGuardArgv(pid)
	if !ok {
		return true
	}
	return argvIsClaudeJob(argv, settingsPath)
}

// reuseGuardArgv reads pid's argv for the PID-reuse guard. A package var so
// tests drive the matcher cross-platform without a live process. Default:
// platformReuseGuardArgv.
var reuseGuardArgv = platformReuseGuardArgv

// platformReuseGuardArgv returns pid's argv, ok=false when it can't be read.
// Linux reads /proc/<pid>/cmdline through procRoot (the test seam); darwin
// shells to ps via procintrospect.Cmdline; other platforms return ok=false so
// processAlive degrades to a bare kill(0).
func platformReuseGuardArgv(pid int) ([]string, bool) {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return nil, false
		}
		return strings.Split(string(data), "\x00"), true
	case "darwin":
		argv, err := procintrospect.Cmdline(pid)
		if err != nil {
			return nil, false
		}
		return argv, true
	}
	return nil, false
}

// argvIsClaudeJob is the shared matcher: argv carries a claude executable token
// (a "/claude/" path segment — versions/<hash> basenames aren't "claude" — or a
// basename containing "claude") AND this job's --settings value. Matching
// --settings alone is deliberate; --model is too loose.
//
// The --settings value is matched as an exact argv token first. But darwin
// recovers argv via `ps -o command=`, which space-splits the command line, so a
// --settings path containing a space would never match as an exact token — the
// live job would be mis-read as dead and GC'd out from under its claude child.
// When the exact-token match misses, fall back to a substring check on the
// space-rejoined argv. (On Linux the NUL-delimited argv always has the exact
// token, so the fallback never fires there.)
func argvIsClaudeJob(argv []string, settingsPath string) bool {
	var hasClaude, hasSettings bool
	for _, arg := range argv {
		if arg == "" {
			continue
		}
		if !hasClaude && (strings.Contains(arg, "/claude/") || strings.Contains(filepath.Base(arg), "claude")) {
			hasClaude = true
		}
		if arg == settingsPath {
			hasSettings = true
		}
	}
	if !hasSettings && settingsPath != "" && strings.Contains(strings.Join(argv, " "), settingsPath) {
		// Darwin lossy-split recovery: the path survived as a substring of the
		// space-joined argv even though it was split across tokens.
		hasSettings = true
	}
	return hasClaude && hasSettings
}

func metaPath(dir, jobID string) string { return filepath.Join(dir, jobID+".json") }

func writeMeta(dir string, m jobMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(dir, m.JobID), data, 0o600)
}

func readMeta(dir, jobID string) (jobMeta, error) {
	var m jobMeta
	data, err := os.ReadFile(metaPath(dir, jobID))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

// removeJob deletes every file in a job's group (best-effort).
func removeJob(dir, jobID string) {
	for _, suffix := range []string{".json", ".out", ".err", ".prompt", ".result.json"} {
		_ = os.Remove(filepath.Join(dir, jobID+suffix))
	}
}

// registerSyncJob records a SYNCHRONOUS Run on the agent-status board so it is
// visible WHILE it executes. It writes only a running jobMeta — NO prompt /
// answer text (key-safety, same discipline as background). PID is the
// current cc-fleet process, so StatusFor's bare kill(0) reports "running" until
// finalizeSyncJob caches the terminal result; SettingsPath is intentionally
// empty so processAlive does NOT apply the claude-cmdline reuse guard to a pid
// that is cc-fleet, not a claude child. best-effort: any error yields an empty
// jobID and the run proceeds unrecorded — board bookkeeping must never fail the
// run or change its returned Result.
func registerSyncJob(req Request, model string) string {
	dir, err := jobsDir()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	jobID := uuid.NewString()
	meta := jobMeta{
		JobID:         jobID,
		PID:           os.Getpid(),
		PGID:          os.Getpid(),
		Vendor:        req.Vendor,
		Model:         model,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Status:        "running",
		OutputFormat:  req.OutputFormat,
		JSON:          req.JSON,
		LeadSessionID: req.LeadSessionID,
		// SettingsPath deliberately empty (see processAlive). Sync writes no .out
		// file, so the deferred result cache is the authoritative done signal.
	}
	if err := writeMeta(dir, meta); err != nil {
		return ""
	}
	return jobID
}

// finalizeSyncJob flips a sync job from running → done/failed by writing a
// SANITIZED terminal result cache: status + vendor/model/started + canonical
// error fields only, with the answer text (res.Result) and Raw STRIPPED so no
// vendor reply is ever persisted to disk for a sync run (the caller already got
// it on stdout). A subsequent StatusFor/ListJobs serves this cache. jobID==""
// (register failed) is a no-op; it is called from a defer so it runs on the
// normal return path that produced res.
func finalizeSyncJob(jobID string, res Result) {
	if jobID == "" {
		return
	}
	dir, err := jobsDir()
	if err != nil {
		return
	}
	meta, _ := readMeta(dir, jobID) // for the stable vendor/model/started columns
	cached := Result{
		OK:             res.OK,
		Vendor:         meta.Vendor,
		Model:          meta.Model,
		JobID:          jobID,
		StartedAt:      meta.StartedAt,
		ErrorCode:      res.ErrorCode,
		ErrorMsg:       res.ErrorMsg,
		Suggestion:     res.Suggestion,
		APIErrorStatus: res.APIErrorStatus,
		LeadSessionID:  meta.LeadSessionID,
	}
	if res.OK {
		cached.Status = "done"
	} else {
		cached.Status = "failed"
	}
	if data, merr := json.Marshal(cached); merr == nil {
		_ = os.WriteFile(filepath.Join(dir, jobID+".result.json"), data, 0o600)
	}
}
