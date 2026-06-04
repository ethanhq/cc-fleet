package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// runsDirName holds run manifests under the jobs dir: ConfigDir/subagent-jobs/runs.
// A manifest <runId>.json is the canonical phase sequencer for a workflow run; the
// member jobs that belong to it carry the same RunID in their own meta. Nesting
// runs/ UNDER the jobs dir keeps GC/PurgeJobs/ListJobs unchanged — they skip
// subdirectories in their readdir filter, so a runs/ entry is already ignored.
const runsDirName = "runs"

// RunPhase is one planned step in a run. Title is the short name a worker passes
// as --phase; Detail is optional free text describing the step.
type RunPhase struct {
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// WorkflowRun is the on-disk manifest for a workflow run, stored at
// ConfigDir/subagent-jobs/runs/<run_id>.json. It records the run's identity and
// its intended phase sequence; the actual subagent jobs are separate files tagged
// with this RunID, joined back in RunStatus.
type WorkflowRun struct {
	RunID     string     `json:"run_id"`
	Name      string     `json:"name,omitempty"`
	StartedAt string     `json:"started_at"`
	Phases    []RunPhase `json:"phases,omitempty"`
	Status    string     `json:"status,omitempty"`
}

// runsDir is ConfigDir/subagent-jobs/runs.
func runsDir() (string, error) {
	dir, err := jobsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runsDirName), nil
}

// NewRun mints a run manifest and persists it. RunID is a fresh uuid; StartedAt is
// RFC3339 UTC (lexically sortable for newest-first listing); Status starts
// "running". The runs dir is created 0o700 and the manifest written 0o600 via the
// atomic-write outlet.
func NewRun(name string, phases []RunPhase) (WorkflowRun, error) {
	dir, err := runsDir()
	if err != nil {
		return WorkflowRun{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: mkdir runs dir: %w", err)
	}
	run := WorkflowRun{
		RunID:     uuid.NewString(),
		Name:      name,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Phases:    phases,
		Status:    "running",
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: marshal run: %w", err)
	}
	if err := fileutil.AtomicWrite(filepath.Join(dir, run.RunID+".json"), data, 0o600); err != nil {
		return WorkflowRun{}, err
	}
	return run, nil
}

// ReadRun loads a manifest by id. runID is validated first because it becomes a
// filesystem path component (guards against a "../" escape via the CLI/status path).
func ReadRun(runID string) (WorkflowRun, error) {
	if err := ids.ValidateJobID(runID); err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: invalid run id %q: %w", runID, err)
	}
	dir, err := runsDir()
	if err != nil {
		return WorkflowRun{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, runID+".json"))
	if err != nil {
		// Canonical, path-free "not found" so an unknown-run id doesn't leak the
		// config-dir layout into the CLI's JSON error envelope (a genuine I/O fault
		// keeps its context for debugging).
		if errors.Is(err, os.ErrNotExist) {
			return WorkflowRun{}, fmt.Errorf("run %q not found", runID)
		}
		return WorkflowRun{}, err
	}
	var run WorkflowRun
	if err := json.Unmarshal(data, &run); err != nil {
		return WorkflowRun{}, fmt.Errorf("subagent: parse run %q: %w", runID, err)
	}
	return run, nil
}

// ListRuns returns every run manifest, newest-first by StartedAt (RFC3339 is
// lexically sortable, so a string descending sort works). A missing runs dir means
// nothing has run yet → (nil, nil). Unparseable manifests are skipped.
func ListRuns() ([]WorkflowRun, error) {
	dir, err := runsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subagent: read runs dir: %w", err)
	}
	var runs []WorkflowRun
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var run WorkflowRun
		if json.Unmarshal(data, &run) != nil {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt > runs[j].StartedAt
	})
	return runs, nil
}

// removeRun deletes a manifest best-effort (used by GC/PurgeJobs manifest pruning).
func removeRun(dir, runID string) {
	_ = os.Remove(filepath.Join(dir, runID+".json"))
}

// RunStatus returns a run's manifest plus the Results of the jobs tagged with it.
// A missing manifest is an error (unknown run). The jobs are ListJobs() filtered
// by RunID, already newest-first.
func RunStatus(runID string) (WorkflowRun, []Result, error) {
	run, err := ReadRun(runID)
	if err != nil {
		return WorkflowRun{}, nil, err
	}
	all, err := ListJobs()
	if err != nil {
		return run, nil, err
	}
	var jobs []Result
	for _, j := range all {
		if j.RunID == run.RunID {
			jobs = append(jobs, j)
		}
	}
	return run, jobs, nil
}
