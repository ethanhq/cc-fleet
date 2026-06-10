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
	"time"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/pinned"
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

	// Workflow run grouping (optional): the run this job belongs to, the phase
	// within it, and a human label — so the board can group jobs into a run tree.
	RunID string `json:"run_id,omitempty"`
	Phase string `json:"phase,omitempty"`
	Label string `json:"label,omitempty"`
	// JournalKey is the leaf's content-hash key, carried so finalizeSyncJob/StatusFor can
	// stamp the terminal Result with it (the board needs it to restart this single leaf).
	JournalKey string `json:"journal_key,omitempty"`
	// Attempt is the 1-based exec ordinal this job last ran at (>1 occurs only in metas
	// from engines that retried schema mismatches); 0 backfills a legacy meta. Carried
	// onto the reconstructed Result.
	Attempt int `json:"attempt,omitempty"`

	// PersistIO records that this job opted into board drill-in, so finalizeSyncJob
	// writes the answer side file (<id>.answer) on completion. The result CACHE stays
	// answer-stripped regardless; the side files are the separate opt-in drill-in source.
	PersistIO bool `json:"persist_io,omitempty"`

	// PromptProfile is the EFFECTIVE profile this job ran (post-version-gate);
	// SlimDowngrade is non-empty when a slim request ran full instead (the reason).
	// Both are backfilled onto every reconstructed Result so a re-classified
	// background leaf keeps the profile/downgrade signal.
	PromptProfile string `json:"prompt_profile,omitempty"`
	SlimDowngrade string `json:"slim_downgrade,omitempty"`
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
func launchBackground(req Request, binaryPath, profilePath, model, effective, downgrade string) Result {
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
	slimPath := filepath.Join(dir, jobID+".slimprompt")

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

	// Render the slim prompt sidecar after the jobID mint, before cmd.Start.
	slim, slimErr := buildSlimArgv(effective, jobID, req, model)
	if slimErr != nil {
		_ = os.Remove(outPath)
		_ = os.Remove(errPath)
		return fail(ErrCodeFailed, slimErr.Error(), req.Vendor, "")
	}

	// Board drill-in: persist the prompt side file before the start (every failure
	// path below already removes it).
	if req.PersistIO && req.IOPrompt != "" {
		_ = os.WriteFile(filepath.Join(dir, jobID+".prompt"), []byte(req.IOPrompt), 0o600)
	}

	argv := buildArgv(binaryPath, profilePath, model, innerReq, slim)
	// Fresh exec.Command (no context) → no deadline; child outlives parent.
	cmd := exec.Command(binaryPath)
	cmd.Args = argv
	cmd.Env = childenv.Clean(os.Environ())
	if effective == ProfileSlimRO {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
	}
	cmd.Stdout = outF
	cmd.Stderr = errF
	setGroupAttr(cmd)

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
				_ = os.Remove(slimPath)
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
		_ = os.Remove(slimPath)
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
		PersistIO: req.PersistIO, // finalize (via StatusFor) writes .answer only when carried here
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
		RunID:         req.RunID,
		Phase:         req.Phase,
		Label:         req.Label,
		JournalKey:    req.JournalKey,
		PromptProfile: effective,
		SlimDowngrade: downgrade,
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
		_ = os.Remove(slimPath)
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
		RunID:         meta.RunID,
		Phase:         meta.Phase,
		Label:         meta.Label,
		JournalKey:    meta.JournalKey,
		PromptProfile: meta.PromptProfile,
		SlimDowngrade: meta.SlimDowngrade,
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

// killProcessGroup reaps the whole process tree rooted at the just-launched
// background child (the leader's process group on unix; the current process tree
// via taskkill /T on Windows): a graceful terminate, a short grace, then a
// forced kill of survivors. Best-effort: an already-gone tree is silently ok. A
// package var only to allow test injection. It is deliberately job-handle-free
// (see killProcessTree) so it only ever runs while the child is still owned by
// this process, before the successful-launch Release.
var killProcessGroup = killProcessTree

// reapJobTree is ReapJob's kill: an ANCESTRY reap rooted at the job's recorded PID. The tree
// walk matters because a SYNC job's recorded PID is its launcher process while the claude child
// is its own group leader (Setpgid) — a bare group kill would miss it; for a background job
// (the detached child IS the leader) the walk degrades to the same group kill. A package var
// only to allow test injection.
var reapJobTree = reapEngineTree

// ReapJob terminates a job's process tree and finalizes it as a timeout failure. The workflow
// runtime uses it to enforce a background leaf's timeout at wait() time (launchBackground itself
// is deadline-less so a detached job survives the launcher); DeleteSession uses it to stop a
// still-live standalone job whose files it removes right after (so the timeout-flavored finalize
// never surfaces there). Path-safe (validates the id) and best-effort: an unknown/gone job is a
// no-op.
func ReapJob(jobID string) error {
	if err := ids.ValidateJobID(jobID); err != nil {
		return err
	}
	dir, err := jobsDir()
	if err != nil {
		return err
	}
	meta, merr := readMeta(dir, jobID)
	if merr != nil {
		return nil // unknown / already gone — nothing to reap
	}
	if meta.PID > 0 {
		reapJobTree(meta.PID)
	}
	finalizeSyncJob(jobID, fail(ErrCodeTimeout, "background leaf exceeded its timeout", meta.Vendor, ""))
	return nil
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
			// Backfill grouping keys for caches written before they existed.
			if r.LeadSessionID == "" {
				r.LeadSessionID = meta.LeadSessionID
			}
			if r.RunID == "" {
				r.RunID = meta.RunID
				r.Phase = meta.Phase
				r.Label = meta.Label
			}
			if r.JournalKey == "" {
				r.JournalKey = meta.JournalKey
			}
			if r.PromptProfile == "" {
				r.PromptProfile = meta.PromptProfile
				r.SlimDowngrade = meta.SlimDowngrade
			}
			if r.Attempt == 0 {
				r.Attempt = meta.Attempt
			}
			return r
		}
	}

	// PID<=0 with no cached result = no terminal signal yet (a queued placeholder before its pool
	// slot). Report it queued — PID is the authority, not the Status string — so the dead-classify
	// path below never reads its empty capture as a done/failed leaf.
	if meta.PID <= 0 {
		return Result{
			OK: true, JobID: jobID, Status: "queued",
			Vendor: meta.Vendor, Model: meta.Model, StartedAt: meta.StartedAt,
			LeadSessionID: meta.LeadSessionID, RunID: meta.RunID, Phase: meta.Phase, Label: meta.Label,
			JournalKey: meta.JournalKey, PromptProfile: meta.PromptProfile, SlimDowngrade: meta.SlimDowngrade,
			Attempt: meta.Attempt,
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
			RunID:         meta.RunID,
			Phase:         meta.Phase,
			Label:         meta.Label,
			JournalKey:    meta.JournalKey,
			PromptProfile: meta.PromptProfile,
			SlimDowngrade: meta.SlimDowngrade,
			Attempt:       meta.Attempt,
		}
	}

	// Dead → classify the captured output. The detached child was Released, so we can't reap a real
	// exit code; the (json) envelope or (text) answer is the only terminal signal.
	outPath := filepath.Join(dir, jobID+".out")
	errPath := filepath.Join(dir, jobID+".err")
	stdout, _ := os.ReadFile(outPath)
	stderr, _ := os.ReadFile(errPath)
	innerJSON := meta.JSON || meta.OutputFormat == "json"
	// A detached json leaf (settingsPath set) can be seen dead a moment before its envelope write
	// lands. When the capture is empty, re-read once after a short delay before classifying, so a
	// late write isn't cached as a failure. Non-empty output is already written and skips the wait.
	if meta.SettingsPath != "" && innerJSON && strings.TrimSpace(string(stdout)) == "" {
		time.Sleep(statusConfirmDelay)
		stdout, _ = os.ReadFile(outPath)
		stderr, _ = os.ReadFile(errPath)
	}
	var res Result
	if vanishedWithoutResult(stdout, innerJSON) {
		// No envelope (json) / no answer (text) and no real exit code: the leaf ended without
		// finishing. Fail honestly (keep any stderr clue) — never bless the synthetic exit as "done".
		res = failVanished(meta.Vendor, stderr)
	} else {
		req := Request{Vendor: meta.Vendor, Model: meta.Model, JSON: meta.JSON, OutputFormat: meta.OutputFormat}
		res = classify(req, meta.Model, stdout, stderr, 0, false, innerJSON)
	}
	res.JobID = jobID
	res.StartedAt = meta.StartedAt
	res.LeadSessionID = meta.LeadSessionID
	res.RunID = meta.RunID
	res.Phase = meta.Phase
	res.Label = meta.Label
	res.JournalKey = meta.JournalKey
	res.PromptProfile = meta.PromptProfile
	res.SlimDowngrade = meta.SlimDowngrade
	res.Attempt = meta.Attempt
	if res.OK {
		res.Status = "done"
	} else {
		res.Status = "failed"
	}
	// Board drill-in: persist the answer side file (mirror finalizeSyncJob, which never
	// sees a detached background job — this dead-classification is its only finalizer).
	if meta.PersistIO && res.Result != "" {
		_ = os.WriteFile(filepath.Join(dir, jobID+".answer"), []byte(res.Result), 0o600)
	}
	// Cache the terminal result (best-effort; a failed cache just re-classifies).
	if data, merr := json.Marshal(res); merr == nil {
		_ = os.WriteFile(resultPath, data, 0o600)
	}
	return res
}

// statusConfirmDelay is how long StatusFor waits before re-reading a just-dead detached
// background leaf's empty capture, to let an envelope write that lands right after the
// process is seen gone become visible. A package var so tests can zero it.
var statusConfirmDelay = 75 * time.Millisecond

// vanishedWithoutResult reports a dead job that left no terminal signal: no parseable envelope
// (json) or no answer text (text). It must classify failed, never a synthetic-exit "done".
func vanishedWithoutResult(stdout []byte, innerJSON bool) bool {
	if innerJSON {
		_, ok := parseInner(stdout)
		return !ok
	}
	return strings.TrimSpace(string(stdout)) == ""
}

// failVanished is the honest terminal failure for a job that ended without a result and whose
// exit status is unknowable (a detached child was Released). It keeps any stderr clue (key-safe
// via stderrPreview) but never claims a clean "exited 0".
func failVanished(vendor string, stderr []byte) Result {
	msg := "subagent ended without a result (process gone; no exit status available)"
	if prev := stderrPreview(stderr); prev != "" {
		msg += ": " + prev
	}
	return fail(ErrCodeFailed, msg, vendor, suggestionFor(ErrCodeFailed))
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
	// Snapshot the pin registry once: a pinned job (or a pinned run's leaf) is kept
	// regardless of age. A read glitch must NOT cause pinned records to be deleted, so a
	// snapshot error fails the GC rather than proceeding pin-blind.
	pins, perr := pinned.Snapshot()
	if perr != nil {
		return fail(ErrCodeFailed, fmt.Sprintf("read pin registry: %v", perr), "", "")
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
		// A user-pinned job — or a leaf of a pinned run — is kept regardless of age, so a
		// pinned run never loses its leaves to the job scan (run↔job coupling).
		if pins.Has(pinned.Job, jobID) || (meta.RunID != "" && pins.Has(pinned.Run, meta.RunID)) {
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
		if !resultCached && (processAlive(meta.PID, meta.SettingsPath) || queuedPlaceholder(meta)) {
			continue // truly running (no result cache + alive process), or a not-yet-started queued leaf
		}
		if started, perr := time.Parse(time.RFC3339, meta.StartedAt); perr == nil && started.After(cutoff) {
			continue // finished but too recent
		}
		removeJob(dir, jobID)
		removed++
	}
	gcRunManifests(dir, cutoff, pins)
	return Result{OK: true, Removed: removed}
}

// gcRunManifests prunes run manifests after the job sweep. A manifest is removed
// iff (a) no surviving job meta still belongs to its run AND (b) the manifest is
// itself older than the same cutoff — so a run with any live/recent member is
// protected, and a freshly created (still-empty) manifest survives until it ages
// out unused. Membership is read FRESH from the jobs dir here (after the job-removal
// pass), not from a snapshot taken before it, so a member that launched mid-GC still
// protects its manifest — closing the readdir-interleaving window where an old run
// gaining a new member could lose its manifest. A manifest that can't be read or
// parsed has no provable recency and (by the membership check) no surviving member,
// so it is treated as an aged orphan and removed (symmetric with purgeRunManifests
// and with the job side, where an unreadable meta is also reaped). Manifest pruning
// is kept OUT of the Removed counter, which counts job groups, not runs.
func gcRunManifests(jobsDir string, cutoff time.Time, pins pinned.Set) {
	dir := filepath.Join(jobsDir, runsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no runs dir → nothing to prune
	}
	live := survivingRunIDs(jobsDir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		runID := strings.TrimSuffix(name, ".json")
		if pins.Has(pinned.Run, runID) {
			continue // user-pinned: kept even when memberless (its leaves were kept too)
		}
		if live[runID] {
			continue // a surviving member protects the manifest
		}
		// No surviving member. Keep only a manifest that PROVES it is still recent
		// (a fresh, not-yet-populated run); an unreadable/unparseable manifest can't
		// prove recency → treated as an aged orphan and removed below.
		if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil {
			var run WorkflowRun
			if json.Unmarshal(data, &run) == nil {
				if runIsRecent(run, cutoff) {
					continue // fresh empty OR actively-resuming manifest → keep
				}
			}
		}
		removeRun(dir, runID)
	}
	sweepOrphanRunSidecars(dir, cutoff, false, pins)
}

// sweepOrphanRunSidecars removes any per-run sidecar (runs/<id>.journal, …) whose
// manifest runs/<id>.json no longer exists. removeRun reaps a run's whole group, so
// an orphan only arises if a prior remove was interrupted mid-group; left behind it
// would waste disk and, at uninstall, keep the runs/ dir non-empty so PurgeJobs could
// never os.Remove it. Best-effort, like the rest of GC bookkeeping.
//
// When force is false (periodic GC) a FRESH orphan (mtime after cutoff) is KEPT: a run
// being launched/recreated by another process can momentarily have a journal whose
// manifest write hasn't landed, and reaping it would lose an active run's cache. This is
// symmetric with the manifest recency rule (runIsRecent). force=true (uninstall purge)
// removes every orphan unconditionally — no run is active during uninstall.
func sweepOrphanRunSidecars(runsDir string, cutoff time.Time, force bool, pins pinned.Set) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		for _, ext := range runSidecarExts {
			if !strings.HasSuffix(name, ext) {
				continue
			}
			base := strings.TrimSuffix(name, ext)
			if pins.Has(pinned.Run, base) {
				continue // user-pinned run: keep its sidecars (periodic GC; uninstall passes an empty set)
			}
			if _, serr := os.Stat(filepath.Join(runsDir, base+".json")); !errors.Is(serr, os.ErrNotExist) {
				continue // manifest present (or unstat-able) → not a removable orphan
			}
			path := filepath.Join(runsDir, name)
			if !force {
				if info, ierr := os.Stat(path); ierr == nil && info.ModTime().After(cutoff) {
					continue // fresh orphan → may belong to an active run; keep it
				}
			}
			_ = os.Remove(path)
		}
	}
}

// survivingRunIDs reads the jobs dir and returns the set of RunIDs that still have
// at least one job meta on disk. gcRunManifests calls it AFTER the job-removal pass,
// so the snapshot reflects which runs still have members (kept or just launched),
// and a manifest is pruned only when its run is genuinely memberless.
func survivingRunIDs(jobsDir string) map[string]bool {
	live := map[string]bool{}
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return live
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		if meta, merr := readMeta(jobsDir, strings.TrimSuffix(name, ".json")); merr == nil && meta.RunID != "" {
			live[meta.RunID] = true
		}
	}
	return live
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

	// runningRuns collects the RunIDs of jobs we keep, so a manifest with a live
	// member is preserved while all others are purged.
	runningRuns := map[string]bool{}
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
			if meta, merr := readMeta(dir, jobID); merr == nil && (processAlive(meta.PID, meta.SettingsPath) || queuedPlaceholder(meta)) {
				running = append(running, jobID)
				if meta.RunID != "" {
					runningRuns[meta.RunID] = true
				}
				continue // live (or a not-yet-started queued leaf) → keep this job's file group
			}
		}
		// Finished (or dead / unreadable) → remove its full file group now, even
		// if OTHER jobs are still running (partial clean).
		removeJob(dir, jobID)
		removedFinished = append(removedFinished, jobID)
	}

	purgeRunManifests(dir, runningRuns)

	sort.Strings(removedFinished)
	sort.Strings(running)

	// Drop the dir when nothing is left running. RemoveAll (not Remove) so a leftover
	// per-run .lock file — deliberately not a GC'd sidecar — can't strand the dir at an
	// exclusive uninstall; with nothing running, no lock is held, so this is safe.
	if len(running) == 0 {
		_ = os.RemoveAll(dir)
	}
	return dir, removedFinished, running, nil
}

// purgeRunManifests removes every run manifest whose RunID has no live member
// (runningRuns), then removes the now-empty runs/ dir best-effort. A missing runs/
// dir is a no-op. Keeping the runs/ dir empty-and-removable is what lets PurgeJobs
// finally os.Remove the jobs dir when nothing is running.
func purgeRunManifests(jobsDir string, runningRuns map[string]bool) {
	dir := filepath.Join(jobsDir, runsDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no runs dir → nothing to purge
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// The filename IS the run id, so membership is decided WITHOUT reading the
		// manifest: a run with a live member is kept even if its manifest is corrupt;
		// every other manifest (no live member, or unreadable / parse-fail) is dropped.
		runID := strings.TrimSuffix(name, ".json")
		if runningRuns[runID] {
			continue // live member → keep this run's manifest
		}
		removeRun(dir, runID)
	}
	sweepOrphanRunSidecars(dir, time.Time{}, true, pinned.Set{}) // uninstall: empty pin set → drop every orphan sidecar so the dir can empty
	_ = os.Remove(dir)                                           // succeeds only when empty (all manifests + sidecars gone)
}

// procRoot is the procfs mount point. A package var so tests can point the
// PID-reuse guard at a fixture tree instead of the live /proc.
var procRoot = "/proc"

// queuedPlaceholder reports whether meta is a workflow queued placeholder the engine minted before
// the leaf got a pool slot (PID=0, Status="queued", no process yet). GC/PurgeJobs treat it as live —
// mirroring StatusFor's queued branch — so a `subagent-gc --older-than 0s` (which clears done entries)
// can't remove an active not-yet-started leaf out from under the engine. It flips to running
// (registerSyncJob) or terminal (finalize) shortly; a crashed engine's orphan is reaped by run prune/rm.
func queuedPlaceholder(meta jobMeta) bool {
	return meta.Status == "queued" && meta.PID == 0
}

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
	if !pidAlive(pid) {
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

// removeJob deletes every file in a job's group (best-effort), including the opt-in
// drill-in side files (.prompt / .answer) and a slim run's prompt sidecar (.slimprompt).
func removeJob(dir, jobID string) {
	for _, suffix := range []string{".json", ".out", ".err", ".prompt", ".answer", ".activity", ".slimprompt", ".result.json"} {
		_ = os.Remove(filepath.Join(dir, jobID+suffix))
	}
}

// leafActivityPath returns <jobID>.activity in the jobs dir — where a SYNC leaf streams its
// per-tool/usage activity when StreamActivity (the board reads it for the Activity feed). The id
// is a freshly-minted uuid from registerSyncJob; the board validates it via ids.ValidateJobID
// before reading.
func leafActivityPath(jobID string) (string, error) {
	dir, err := jobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, jobID+".activity"), nil
}

// slimPromptPath returns <jobID>.slimprompt in the jobs dir — the per-job sidecar
// holding a slim run's rendered system prompt (consumed via --system-prompt-file
// and reaped with the job). Mirrors leafActivityPath.
func slimPromptPath(jobID string) (string, error) {
	dir, err := jobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, jobID+".slimprompt"), nil
}

// mintSyncJobID creates the jobs dir and returns a fresh job id, BEFORE buildArgv
// so a slim sync run can write its <jobID>.slimprompt sidecar and reference it.
// Best-effort: "" on any error, like registerSyncJob — board bookkeeping never
// fails the run.
func mintSyncJobID() string {
	dir, err := jobsDir()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return uuid.NewString()
}

// registerSyncJob records a SYNCHRONOUS Run on the Agents Board so it is
// visible WHILE it executes. It writes only a running jobMeta — NO prompt /
// answer text (key-safety, same discipline as background). PID is the
// current cc-fleet process, so StatusFor's bare kill(0) reports "running" until
// finalizeSyncJob caches the terminal result; SettingsPath is intentionally
// empty so processAlive does NOT apply the claude-cmdline reuse guard to a pid
// that is cc-fleet, not a claude child. jobID is minted by mintSyncJobID before
// buildArgv; an empty jobID (mint failed) leaves the run unrecorded. best-effort:
// any error proceeds unrecorded — board bookkeeping must never fail the run or
// change its returned Result.
//
// It returns whether the meta was written: a FAILED registration must skip
// finalizeSyncJob (which would otherwise write an orphan .result.json with no
// backing meta) and let Run reap the slim sidecar instead.
func registerSyncJob(jobID string, req Request, model string, effective, downgrade string) bool {
	if jobID == "" {
		return false
	}
	dir, err := jobsDir()
	if err != nil {
		return false
	}
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
		RunID:         req.RunID,
		Phase:         req.Phase,
		Label:         req.Label,
		JournalKey:    req.JournalKey,
		Attempt:       req.Attempt,
		PersistIO:     req.PersistIO,
		PromptProfile: effective,
		SlimDowngrade: downgrade,
		// SettingsPath deliberately empty (see processAlive). Sync writes no .out
		// file, so the deferred result cache is the authoritative done signal.
	}
	// A REUSED job id (the engine's queued→running flip) must start clean: drop any terminal
	// cache + stale answer/activity so the board re-reads this job as running, not a prior
	// done/answer. No-op for a fresh id.
	for _, ext := range []string{".result.json", ".answer", ".activity"} {
		_ = os.Remove(filepath.Join(dir, jobID+ext))
	}
	if err := writeMetaFn(dir, meta); err != nil {
		return false
	}
	// Opt-in board drill-in: persist the prompt to a 0600 side file. Content-privacy,
	// not key-safety (the vendor key never enters the prompt). Best-effort — a write
	// hiccup just means no prompt in the detail card, never a failed run.
	if req.PersistIO && req.IOPrompt != "" {
		_ = os.WriteFile(filepath.Join(dir, jobID+".prompt"), []byte(req.IOPrompt), 0o600)
	}
	return true
}

// MintQueuedLeaf records a leaf the workflow engine has admitted but not yet given a pool slot:
// a PLACEHOLDER jobMeta with Status="queued" and PID=0 (no process yet), so the board shows it as
// a queued ◌ row immediately instead of only once it starts running. The engine passes the returned
// id back as Request.JobID, so the SAME on-disk job flips queued→running (registerSyncJob) →terminal
// (finalizeSyncJob) as one file. It writes NO prompt/answer (key-safety, same as registerSyncJob).
// Best-effort: "" on any error, so board bookkeeping never fails the run; an empty id makes the
// engine fall back to minting at run time.
func MintQueuedLeaf(req Request, model string) string {
	jobID := mintSyncJobID()
	if jobID == "" {
		return ""
	}
	dir, err := jobsDir()
	if err != nil {
		return ""
	}
	meta := jobMeta{
		JobID:         jobID,
		PID:           0, // no process yet — StatusFor reports queued until registerSyncJob flips it
		Vendor:        req.Vendor,
		Model:         model,
		StartedAt:     time.Now().UTC().Format(time.RFC3339),
		Status:        "queued",
		OutputFormat:  req.OutputFormat,
		JSON:          req.JSON,
		LeadSessionID: req.LeadSessionID,
		RunID:         req.RunID,
		Phase:         req.Phase,
		Label:         req.Label,
		JournalKey:    req.JournalKey,
		Attempt:       req.Attempt,
		PersistIO:     req.PersistIO,
		PromptProfile: req.PromptProfile,
	}
	if err := writeMetaFn(dir, meta); err != nil {
		return ""
	}
	return jobID
}

// FinalizeQueuedLeafFailed marks a leaf's (reused) job terminal-failed when the engine abandoned it
// without a success: a queued placeholder cancelled before its slot or whose worktree failed, a
// pre-flight vendor failure (subagent.Run returned before registering), or a schema-invalid leaf
// whose exec cached "done". A res carrying a real error class is preserved (so the board keeps
// e.g. INSUFFICIENT_BALANCE); otherwise a canonical SUBAGENT_FAILED is written. No-op for an empty id.
func FinalizeQueuedLeafFailed(jobID string, res Result) {
	if jobID == "" {
		return
	}
	if res.OK || res.ErrorCode == "" {
		res = fail(ErrCodeFailed, "leaf did not complete", res.Vendor, "")
	}
	finalizeSyncJob(jobID, res)
}

// finalizeSyncJob flips a sync job from running → done/failed by writing a
// SANITIZED terminal result cache: status + vendor/model/started + the SAFE
// metrics (Usage / cost / turns / duration — claude's own metering, which the
// board's Workflows view needs to show a done leaf's tokens + "done · N turns"
// outcome) + canonical error fields, with the answer text (res.Result) and Raw
// STRIPPED so no vendor reply is ever persisted to disk for a sync run (the caller
// already got it on stdout). A subsequent StatusFor/ListJobs serves this cache.
// jobID=="" (register failed) is a no-op; it is called from a defer so it runs on
// the normal return path that produced res.
func finalizeSyncJob(jobID string, res Result) {
	if jobID == "" {
		return
	}
	dir, err := jobsDir()
	if err != nil {
		return
	}
	meta, _ := readMeta(dir, jobID) // for the stable vendor/model/started columns
	// Opt-in board drill-in: persist the answer to a 0600 side file, SEPARATE from the
	// cache below (which stays answer-stripped so the board TABLE never shows a reply).
	// Content-privacy, not key-safety. Best-effort; only a real answer (success) is kept.
	if meta.PersistIO && res.Result != "" {
		_ = os.WriteFile(filepath.Join(dir, jobID+".answer"), []byte(res.Result), 0o600)
	}
	cached := Result{
		OK:        res.OK,
		Vendor:    meta.Vendor,
		Model:     meta.Model,
		JobID:     jobID,
		StartedAt: meta.StartedAt,
		// Safe final metrics (claude's own metering, NOT the answer) — the board needs them
		// for a done leaf's token/cost/turns columns and the "done · N turns" outcome.
		Usage:          res.Usage,
		CostUSD:        res.CostUSD,
		NumTurns:       res.NumTurns,
		DurationMs:     res.DurationMs,
		StopReason:     res.StopReason,
		ErrorCode:      res.ErrorCode,
		ErrorMsg:       res.ErrorMsg,
		Suggestion:     res.Suggestion,
		APIErrorStatus: res.APIErrorStatus,
		LeadSessionID:  meta.LeadSessionID,
		RunID:          meta.RunID,
		Phase:          meta.Phase,
		Label:          meta.Label,
		JournalKey:     meta.JournalKey,
		Attempt:        meta.Attempt,
		PromptProfile:  meta.PromptProfile,
		SlimDowngrade:  meta.SlimDowngrade,
	}
	switch {
	case res.OK:
		cached.Status = "done"
	case res.ErrorCode == ErrCodeStopped:
		cached.Status = "stopped" // a `workflow stop` reap — terminal, but not a failure
	default:
		cached.Status = "failed"
	}
	if data, merr := json.Marshal(cached); merr == nil {
		_ = os.WriteFile(filepath.Join(dir, jobID+".result.json"), data, 0o600)
	}
}
