package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
)

// savedWorkflowsDirName holds NAMED, reusable workflows under ConfigDir — distinct from the ephemeral
// per-run runs/<id>.star. A user saves a run from the board (s + a name), and a later invocation
// re-runs it by name (workflow run --saved <name>). Each <name> has a <name>.star (the script, copied
// from the run) and a <name>.json (metadata). The .star is workflow LOGIC — no secret (keys flow only
// via apiKeyHelper) — so 0600 is content-privacy, not key-safety.
const savedWorkflowsDirName = "workflows"

// SavedWorkflow is one saved workflow's metadata (workflows/<name>.json). SessionID records the
// session it was saved from so discovery can surface the current session's saves first.
type SavedWorkflow struct {
	Name        string `json:"name"`
	RunID       string `json:"run_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Description string `json:"description,omitempty"`
	SavedAt     string `json:"saved_at"`
}

// savedNameRe bounds a saved-workflow name to a path-safe slug (it becomes a filename): letters,
// digits, dot, dash, underscore — 1..64 chars.
var savedNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidSavedName reports whether name is a path-safe saved-workflow name. A leading dot, "." or ".."
// is rejected so a name can never escape the workflows dir or shadow a dotfile.
func ValidSavedName(name string) bool {
	return savedNameRe.MatchString(name) && !strings.HasPrefix(name, ".")
}

func savedWorkflowsDir() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, savedWorkflowsDirName), nil
}

// savedPath returns workflows/<name><ext>, validating the name first (it becomes a path component).
func savedPath(name, ext string) (string, error) {
	if !ValidSavedName(name) {
		return "", fmt.Errorf("subagent: invalid workflow name %q (use letters, digits, . _ - ; max 64)", name)
	}
	dir, err := savedWorkflowsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+ext), nil
}

// SaveWorkflow copies a run's saved script (runs/<runID>.star) to a NAMED, reusable workflow
// (workflows/<name>.star) plus its metadata, overwriting an existing same-name save (re-save). It
// errors if the run has no saved script. sessionID + description are recorded for discovery.
func SaveWorkflow(runID, name, sessionID, description string) error {
	src, err := RunScriptPath(runID)
	if err != nil {
		return err
	}
	data, rerr := os.ReadFile(src)
	if rerr != nil {
		return fmt.Errorf("subagent: run %s has no saved script to save: %w", runID, rerr)
	}
	dir, derr := savedWorkflowsDir()
	if derr != nil {
		return derr
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	star, err := savedPath(name, ".star")
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWrite(star, data, 0o600); err != nil {
		return err
	}
	meta, _ := json.MarshalIndent(SavedWorkflow{
		Name: name, RunID: runID, SessionID: sessionID, Description: description,
		SavedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	jsonPath, _ := savedPath(name, ".json")
	return fileutil.AtomicWrite(jsonPath, meta, 0o600)
}

// SavedWorkflowScript returns a saved workflow's .star path, erroring if the name is invalid or absent.
// Used by `workflow run --saved <name>`.
func SavedWorkflowScript(name string) (string, error) {
	star, err := savedPath(name, ".star")
	if err != nil {
		return "", err
	}
	if _, serr := os.Stat(star); serr != nil {
		return "", fmt.Errorf("subagent: no saved workflow named %q", name)
	}
	return star, nil
}

// ListSavedWorkflows reads the saved-workflows dir, newest-first by SavedAt. A missing dir → (nil, nil);
// unparseable metadata is skipped.
func ListSavedWorkflows() ([]SavedWorkflow, error) {
	dir, err := savedWorkflowsDir()
	if err != nil {
		return nil, err
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if errors.Is(rerr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subagent: read saved workflows: %w", rerr)
	}
	var out []SavedWorkflow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, derr := os.ReadFile(filepath.Join(dir, e.Name()))
		if derr != nil {
			continue
		}
		var sw SavedWorkflow
		if json.Unmarshal(data, &sw) == nil && sw.Name != "" {
			out = append(out, sw)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SavedAt > out[j].SavedAt })
	return out, nil
}
