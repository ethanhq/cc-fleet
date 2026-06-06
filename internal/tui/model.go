// Package tui implements the interactive terminal UI shown when cc-fleet is
// run bare (no subcommand) from an interactive terminal. It is a thin
// arrow-key front end over the same internal packages the subcommands use
// (userops for vendor CRUD, teardown for teammate discovery) so the two never
// drift. It is gated behind a tty check in cmd/cc-fleet so pipes, CI, and
// --json callers never block on the bubbletea event loop.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/panevis"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teardown"
	"github.com/ethanhq/cc-fleet/internal/userops"
	"github.com/ethanhq/cc-fleet/internal/workflow"
)

// screen enumerates the TUI's views; one Model dispatches Update/View on the
// active screen.
type screen int

const (
	screenList screen = iota // the home/hub: Vendors list + inline "+ Add" row
	screenSpawn
	screenWorkflows // RunID-tagged subagent jobs as a live run→phase→agent tree
	screenPickTemplate
	screenForm
	screenModelPick
	screenRemoveConfirm
	screenResult
	screenKeys           // EDIT form → "Manage API keys →": per-vendor multi-key manager
	screenTeammateDetail // board → enter on a teammate: full-field detail card
	screenSetupTmux      // first-run tmux setup nudge; shown before agent-teams/hub
	screenSetup          // first-run agent-teams setup nudge; shown before the hub
)

// formMode records whether the active form is an add or an edit so submit
// knows which userops call to make.
type formMode int

const (
	modeAdd formMode = iota
	modeEdit
)

// wfMode is the active pane level of the Workflows master-detail board — all WITHIN
// screenWorkflows, so the refresh/tick/control msg ownership stays on one screen. →/enter
// descend a level; ← ascends but CLAMPS at the board's top level (a no-op there), while esc/tab
// leave for Vendors — so repeated ← can't fall out of the board (mirrors the native board).
type wfMode int

const (
	wfModePicker wfMode = iota // run picker; shown only when >1 run
	wfModePhases               // L1: Phases | the selected phase's agents
	wfModeAgent                // L2: agent list | the selected agent's inline detail (j/k scroll)
)

// Model is the root bubbletea model.
type Model struct {
	screen screen
	width  int
	height int

	// Vendor data, loaded for the Vendors list (the hub) and reused to seed the
	// edit form. vendorCursor ranges over [0, len(vendors)]; the final index is
	// the trailing "+ Add vendor…" row.
	vendors      []userops.VendorView
	vendorsErr   error
	vendorCursor int

	// Add-wizard template picker.
	tmplCursor int

	// Agent-status board (screenSpawn): live teammates + async subagent jobs.
	// spawnCursor selects a teammate row (h/s act on it); job rows are read-only.
	// boardEpoch tags each auto-refresh tick chain so re-entering the board
	// supersedes a stale chain instead of stacking a second one.
	teammates     []teardown.Teammate
	spawnErr      error
	jobs          []subagent.Result
	sessionTitles map[string]string
	spawnCursor   int
	boardEpoch    int
	// boardStatus is a one-line outcome of the last inline hide/show (so a failed
	// h/s surfaces its reason instead of relying on the next silent refresh);
	// boardStatusErr styles it as an error vs an ok confirmation.
	boardStatus    string
	boardStatusErr bool

	// Workflows board (screenWorkflows): a native-mirror master-detail. wfMode re-roots
	// the two panes (run picker → Phases overview → agent detail) WITHIN this one screen;
	// focusedRunID is the run shown in L1/L2; the three cursors index the run picker, the
	// focused run's phases, and the focused phase's agents. wfActivity holds each leaf's
	// activity snapshot (read off the refresh goroutine, keyed by job id): a running leaf's
	// tokens climb live, and every leaf's tool count persists. workflowsEpoch tags each auto-refresh
	// tick chain so re-entering supersedes a stale chain (mirrors boardEpoch).
	// workflowStatus surfaces a stop/restart/save outcome (like boardStatus).
	workflowJobs      []subagent.Result
	workflowRuns      []subagent.WorkflowRun
	workflowsErr      error
	workflowsEpoch    int
	wfMode            wfMode
	focusedRunID      string
	wfRunCursor       int
	wfPhaseCursor     int
	wfAgentCursor     int
	wfActivity        map[string]activitySnapshot
	workflowStatus    string
	workflowStatusErr bool
	// wfDeleteArm holds the run id armed for deletion: the first `d` arms it (status prompt), a second
	// `d` on the same run confirms; any other key disarms (guards against an accidental single keypress).
	wfDeleteArm string
	// Save-workflow name prompt: `s` on a focused run opens wfSaveInput (prefilled with the run name);
	// while wfSaving, keys route to the input (enter saves to ~/.config/cc-fleet/workflows/<name>.star,
	// esc cancels).
	wfSaveInput textinput.Model
	wfSaving    bool

	// Focused-agent inline detail (wfModeAgent right pane): the focused leaf's prompt/answer
	// read from its io files (PersistIO-gated), rendered scrollable in the right pane.
	// wfDetailJob records WHICH leaf the loaded io belongs to, so a render only shows the
	// prompt/output when it matches the focused leaf. wfDetailIO records whether the io files
	// were present. wfCardScroll is the right-pane scroll offset (lines), preserved across the
	// auto-refresh and reset when the focused leaf changes. wfDetailNonce is bumped on each
	// focused-leaf change so a slow read for a prior leaf is dropped, never shown on the wrong one.
	wfDetailJob    subagent.Result
	wfDetailPrompt string
	wfDetailAnswer string
	wfDetailIO     bool
	wfCardScroll   int
	wfDetailNonce  int
	// wfPromptExpanded toggles the inline detail's prompt between a collapsed "N lines · ⏎ expand"
	// summary (default) and the full text; reset to collapsed when the focused leaf changes.
	wfPromptExpanded bool

	// Active add/edit form.
	form     form
	formMode formMode
	editName string

	// Model picker: models fetched from the vendor's models_endpoint to fill the
	// default_model field. While loading, modelList is nil and modelsErr is nil;
	// the picker view branches on those. modelFilter is the live type-to-narrow
	// query; modelCursor indexes the FILTERED list, not modelList.
	modelList   []models.Model
	modelCursor int
	modelsErr   error
	modelFilter string

	// Remove confirmation target.
	removeName string

	// Key manager (screenKeys), reached from the EDIT form's "Manage API keys →"
	// action. keys holds the in-memory key set — full keys live here but the view
	// renders ONLY secrets.MaskKey. keyCursor ranges over [0, len(keys)] (the last
	// index is the "+ Add key…" row). keyEditing is true while the password input
	// is active; keyEditIdx is the entry being edited (-1 = adding). keyRotation
	// mirrors the vendor's current strategy for the header + cycle.
	keys        []secrets.KeyEntry
	keyCursor   int
	keyVendor   string
	keyInput    textinput.Model
	keyEditIdx  int
	keyEditing  bool
	keyRotation string
	keyErr      string

	// Result screen contents.
	result    string
	resultErr bool

	// First-run setup nudges. setupCursor/tmuxCursor select an option on the
	// agent-teams / tmux screens respectively. setupMsg, once non-empty, replaces
	// the agent-teams options with a one-line outcome (e.g. the "restart claude"
	// note after enabling) that any key dismisses. postQuitNote is printed by
	// tui.Run AFTER the program exits — used by the tmux screen's "install it"
	// choice to leave the install command on screen.
	setupCursor  int
	setupMsg     string
	tmuxCursor   int
	postQuitNote string

	loading  bool
	quitting bool
}

// NewModel returns the initial model. It normally parks on the Vendors list
// (the hub) with loading=true so Init can kick off the vendor load. On a first
// run where agent-teams looks unconfigured (and the user hasn't dismissed the
// nudge), it instead opens on the agent-teams setup screen; the hub loads when
// the user leaves setup via toList.
//
// NewModel is only ever called from tui.Run, which cmd/cc-fleet gates to the
// bare-interactive both-TTY path — so the onboarding probe here never runs for
// spawn/subagent/piped/agent callers.
func NewModel() Model {
	switch {
	case onboarding.NeedsTmuxSetup():
		return Model{screen: screenSetupTmux}
	case onboarding.NeedsAgentTeamsSetup():
		return Model{screen: screenSetup}
	default:
		return Model{screen: screenList, loading: true}
	}
}

// Init satisfies tea.Model: load the vendor list so the home screen is
// populated as soon as the program starts. On a setup screen there's nothing to
// load yet — toList kicks off loadVendors when the user proceeds.
func (m Model) Init() tea.Cmd {
	if m.screen == screenSetup || m.screen == screenSetupTmux {
		return nil
	}
	return loadVendors
}

// ---------------------------------------------------------------------------
// messages + commands
// ---------------------------------------------------------------------------

// vendorsMsg carries the result of a userops.List call. It opts into
// screenOwnedAsyncMsg (owningScreen → screenList) so a late result arriving
// after the user navigated away can't clobber m.loading / m.vendors /
// m.vendorsErr — the vendor list only ever loads from screenList.
type vendorsMsg struct {
	vendors []userops.VendorView
	err     error
}

func (vendorsMsg) owningScreen() screen { return screenList }

// boardMsg carries one agent-status board refresh: the discovered teammates
// (health + hidden annotated) and the async subagent jobs. It opts into
// screenOwnedAsyncMsg (owningScreen → screenSpawn) AND carries the boardEpoch
// that scheduled it, so a stale refresh from a prior board visit is dropped
// when the user re-enters (epoch++) or leaves the board.
type boardMsg struct {
	teammates     []teardown.Teammate
	teamErr       error
	jobs          []subagent.Result
	sessionTitles map[string]string
	epoch         int
}

func (boardMsg) owningScreen() screen { return screenSpawn }

// boardTickMsg drives the board's auto-refresh. epoch identifies the tick chain
// that scheduled it; a tick whose epoch != Model.boardEpoch is stale (the user
// left and re-entered the board) and is dropped instead of rescheduling.
type boardTickMsg struct{ epoch int }

// boardRefreshInterval is the auto-refresh cadence while the board is open.
const boardRefreshInterval = 3 * time.Second

// workflowsLiveInterval is the tighter cadence the Workflows board ticks at while a leaf is running,
// so its live token/tool counters climb smoothly instead of in coarse 3s steps; it falls back to
// boardRefreshInterval once nothing is running.
const workflowsLiveInterval = 500 * time.Millisecond

// opDoneMsg carries the result of an add/edit/remove mutation.
type opDoneMsg struct {
	verb string // "add" | "edit" | "remove"
	name string
	err  error
}

// loadVendors is a tea.Cmd (func() tea.Msg) that reads the vendor list.
func loadVendors() tea.Msg {
	res, err := userops.List()
	if err != nil {
		return vendorsMsg{err: err}
	}
	return vendorsMsg{vendors: res.Vendors}
}

// loadBoard returns a tea.Cmd that assembles a board refresh tagged with the
// caller's epoch: discover teammates, annotate them with pane-scan health + the
// hidden flag from team config, and list subagent jobs. A discovery error
// skips annotation (we can't enrich an empty list); a jobs error degrades to
// no jobs — the board never crashes on a data-source failure. The epoch carries
// through to boardMsg so Update can drop a stale refresh from a prior visit.
func loadBoard(epoch int) tea.Cmd {
	return func() tea.Msg {
		items, err := teardown.DiscoverTeammates()
		if err == nil {
			items = teardown.AnnotateHealth(items)
			items = teardown.AnnotateHidden(items)
			items = teardown.AnnotateLeadSession(items)
		}
		jobs, _ := subagent.ListJobs()
		return boardMsg{
			teammates:     items,
			teamErr:       err,
			jobs:          jobs,
			sessionTitles: sessiontitle.Resolve(leadSessionIDs(items, jobs)),
			epoch:         epoch,
		}
	}
}

func leadSessionIDs(teammates []teardown.Teammate, jobs []subagent.Result) []string {
	ids := make([]string, 0, len(teammates)+len(jobs))
	for _, t := range teammates {
		if t.LeadSessionID != "" {
			ids = append(ids, t.LeadSessionID)
		}
	}
	for _, j := range jobs {
		if j.LeadSessionID != "" {
			ids = append(ids, j.LeadSessionID)
		}
	}
	return ids
}

// groupByTeam returns ts stably sorted by LeadSessionID, then Team, so the board
// renders session → team → members. Session order is the earliest
// teammate SpawnTime observed for that session; empty sessions sort last. Team
// order is the earliest SpawnTime within that session. Stable sorting preserves
// input order as the final tiebreaker, and the cursor remains a flat teammate
// index into the returned order.
func groupByTeam(ts []teardown.Teammate) []teardown.Teammate {
	out := make([]teardown.Teammate, len(ts))
	copy(out, ts)

	type orderKey struct {
		firstIdx int
		minTime  int64
		hasTime  bool
	}
	sessionOrder := map[string]orderKey{}
	teamOrder := map[string]orderKey{}
	updateOrder := func(m map[string]orderKey, key string, idx int, spawnTime int64) {
		cur, ok := m[key]
		if !ok {
			cur = orderKey{firstIdx: idx}
		}
		if spawnTime > 0 && (!cur.hasTime || spawnTime < cur.minTime) {
			cur.minTime = spawnTime
			cur.hasTime = true
		}
		m[key] = cur
	}
	for i, t := range ts {
		updateOrder(sessionOrder, t.LeadSessionID, i, t.SpawnTime)
		updateOrder(teamOrder, t.LeadSessionID+"\x00"+t.Team, i, t.SpawnTime)
	}
	lessOrder := func(a, b orderKey) bool {
		if a.hasTime != b.hasTime {
			return a.hasTime
		}
		if a.hasTime && a.minTime != b.minTime {
			return a.minTime < b.minTime
		}
		return a.firstIdx < b.firstIdx
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.LeadSessionID != b.LeadSessionID {
			if a.LeadSessionID == "" {
				return false
			}
			if b.LeadSessionID == "" {
				return true
			}
			return lessOrder(sessionOrder[a.LeadSessionID], sessionOrder[b.LeadSessionID])
		}
		if a.Team != b.Team {
			return lessOrder(teamOrder[a.LeadSessionID+"\x00"+a.Team], teamOrder[b.LeadSessionID+"\x00"+b.Team])
		}
		return false
	})
	return out
}

// boardTick schedules the next auto-refresh tick for the given epoch.
func boardTick(epoch int) tea.Cmd {
	return tea.Tick(boardRefreshInterval, func(time.Time) tea.Msg {
		return boardTickMsg{epoch: epoch}
	})
}

// workflowsMsg carries one Workflows-board refresh: the RunID-tagged subagent jobs, the run
// manifests, and each leaf's activity snapshot (tool calls + a running leaf's live tokens, read off
// the refresh goroutine — render helpers stay pure). It opts into screenOwnedAsyncMsg
// (owningScreen → screenWorkflows) AND carries the workflowsEpoch that scheduled it, so a stale
// refresh from a prior visit is dropped on re-entry (epoch++) or leaving.
type workflowsMsg struct {
	jobs           []subagent.Result
	runs           []subagent.WorkflowRun
	activity       map[string]activitySnapshot // job id → per-leaf tool snapshot (+ live usage while running)
	sessionTitles  map[string]string           // launching-session id → /rename title (for the picker headers)
	titlesResolved bool                        // set only on entry / manual-refresh loads; a live tick leaves m.sessionTitles untouched
	epoch          int
	err            error
}

func (workflowsMsg) owningScreen() screen { return screenWorkflows }

// workflowsTickMsg drives the Workflows board's auto-refresh; a tick whose epoch
// != Model.workflowsEpoch is stale and dropped (mirror boardTickMsg).
type workflowsTickMsg struct{ epoch int }

// loadWorkflows returns a tea.Cmd that assembles a Workflows refresh tagged with the caller's epoch:
// the RunID-tagged subagent jobs (RunID == "" jobs stay on the agent-status board), the run
// manifests, and — for each leaf — its activity snapshot (tool calls + a running leaf's live tokens
// from the <jobID>.activity sidecar). ALL disk reads happen here on the refresh goroutine; render
// helpers stay pure. It carries the first non-nil manifest/jobs error so a data-source failure surfaces.
func loadWorkflows(epoch int, resolve bool) tea.Cmd {
	return func() tea.Msg {
		all, jErr := subagent.ListJobs()
		var jobs []subagent.Result
		activity := map[string]activitySnapshot{}
		for _, j := range all {
			if j.RunID == "" {
				continue
			}
			jobs = append(jobs, j)
			// Read each leaf's activity sidecar once per refresh (keyed by job id): a running leaf's
			// live tokens come from it, and the tool-call count + signatures persist after done (the
			// final Result doesn't carry tool calls). Disk reads stay HERE — render helpers are pure.
			if j.JobID != "" {
				if snap, ok := readLeafActivity(j.JobID); ok {
					activity[j.JobID] = snap
				}
			}
		}
		runs, rErr := subagent.ListRuns()
		err := jErr
		if err == nil {
			err = rErr
		}
		// Resolve the runs' launching-session /rename titles so the picker shows the conversation name
		// (the agent-status board resolves only teammate sessions, not workflow-only ones). Each Lookup
		// scans the projects tree and titles change rarely, so only the entry / manual-refresh loads
		// resolve; a live tick carries no titles and leaves m.sessionTitles as the last resolve left it.
		var titles map[string]string
		if resolve {
			var sids []string
			seen := map[string]bool{}
			for _, r := range runs {
				if r.SessionID != "" && !seen[r.SessionID] {
					seen[r.SessionID] = true
					sids = append(sids, r.SessionID)
				}
			}
			titles = sessiontitle.Resolve(sids)
		}
		return workflowsMsg{jobs: jobs, runs: runs, activity: activity, sessionTitles: titles, titlesResolved: resolve, epoch: epoch, err: err}
	}
}

// stopRunCmd reaps + stops the focused run and reports the outcome on the
// board status line. It is the board's only run-state mutation besides restart. The
// epoch stamps the originating Workflows visit so a result landing after the user
// left + re-entered (epoch++) is dropped (mirror workflowsMsg's gate).
func stopRunCmd(runID string, epoch int) tea.Cmd {
	return func() tea.Msg {
		_, err := subagent.StopRun(runID)
		return workflowCtlMsg{verb: "stop", runID: runID, err: err, epoch: epoch}
	}
}

// restartCmd restarts a run via workflow.Restart: an empty journalKey resumes the WHOLE run
// (re-running only un-journaled / failed leaves); a leaf's journalKey drops just that leaf's cache
// so the resume re-runs only it (+ any downstream leaf whose input shifted). workflow.Restart stops
// a live run first, replays the run's original launch options off the manifest, and (for a keyed
// restart) invalidates the leaf's journal entry. epoch gates a stale result like stopRunCmd.
func restartCmd(runID, journalKey string, epoch int) tea.Cmd {
	return func() tea.Msg {
		err := workflow.Restart(context.Background(), runID, journalKey)
		return workflowCtlMsg{verb: "restart", runID: runID, err: err, epoch: epoch}
	}
}

// deleteRunCmd removes a run + all its jobs from the board (the board never auto-clears, so runs
// accumulate until deleted). Mirrors stopRunCmd's epoch-gated workflowCtlMsg outcome.
func deleteRunCmd(runID string, epoch int) tea.Cmd {
	return func() tea.Msg {
		err := subagent.PurgeRun(runID)
		return workflowCtlMsg{verb: "delete", runID: runID, err: err, epoch: epoch}
	}
}

// workflowCtlMsg carries the outcome of an x/r/d/s control on the Workflows board. Its
// handler records the status line and reloads (mirror paneVisMsg). Owned by
// screenWorkflows; epoch is the originating visit so a stale result is dropped.
type workflowCtlMsg struct {
	verb  string // "stop" | "restart" | "delete" | "save"
	runID string
	err   error
	epoch int
}

func (workflowCtlMsg) owningScreen() screen { return screenWorkflows }

// wfDetailMsg carries the focused leaf's io read for the inline agent-detail pane: the prompt +
// answer (already read off the Update goroutine) and whether either io file was present. Owned by
// screenWorkflows (the agent detail is inline); nonce is the focused-leaf request it answers, so a
// slow read for a previously-focused leaf is dropped rather than shown on the wrong agent.
type wfDetailMsg struct {
	nonce   int
	job     subagent.Result
	prompt  string
	answer  string
	present bool
}

func (wfDetailMsg) owningScreen() screen { return screenWorkflows }

// loadLeafIOCmd reads the selected leaf's prompt/answer side files
// (<ConfigDir>/subagent-jobs/<jobID>.prompt / .answer; 0600, present only when
// persist-io was on). A read failure (absent files) degrades to empty + present
// false so the inline detail shows the not-persisted note. The answer text reaches ONLY the
// focused agent's inline detail pane — never the board's agent rows. nonce tags the request so a
// stale read can't populate a later leaf's detail.
func loadLeafIOCmd(job subagent.Result, nonce int) tea.Cmd {
	return func() tea.Msg {
		prompt, answer, present := readLeafIO(job.JobID)
		return wfDetailMsg{nonce: nonce, job: job, prompt: prompt, answer: answer, present: present}
	}
}

// workflowsTick schedules the next Workflows auto-refresh tick for the epoch at the given cadence
// (tighter while a leaf runs — see workflowsTickInterval).
func workflowsTick(epoch int, interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(time.Time) tea.Msg {
		return workflowsTickMsg{epoch: epoch}
	})
}

// workflowsTickInterval picks the refresh cadence: the tight live interval while any leaf is still
// running (smooth counters), the slow fallback otherwise.
func (m Model) workflowsTickInterval() time.Duration {
	if m.anyLeafRunning() {
		return workflowsLiveInterval
	}
	return boardRefreshInterval
}

// anyLeafRunning reports whether any workflow leaf is still running (drives the live tick cadence).
func (m Model) anyLeafRunning() bool {
	for _, j := range m.workflowJobs {
		if j.Status == "running" {
			return true
		}
	}
	return false
}

// paneVisMsg carries the outcome of an inline hide/show so the board can surface
// a failure (its code/reason/suggestion) instead of silently relying on the next
// refresh to show an unchanged HIDDEN column. Its handler records the status
// line and then reloads the board to reflect the new state.
type paneVisMsg struct{ res panevis.Result }

// hideTeammateCmd hides the selected teammate row's pane and reports the
// panevis.Result so the board can surface success/failure; the result handler
// triggers the reload. It takes the full Teammate struct and forwards its
// Socket + PaneID to HideRef, so socket-aware tmux ops route to the right
// server and a duplicate-name / stale-config row can't mis-target another pane.
func hideTeammateCmd(t teardown.Teammate) tea.Cmd {
	return func() tea.Msg { return paneVisMsg{res: panevis.HideRef(t.Team, t.Name, t.Socket, t.PaneID)} }
}

// showTeammateCmd is the show-side analog of hideTeammateCmd.
func showTeammateCmd(t teardown.Teammate) tea.Cmd {
	return func() tea.Msg { return paneVisMsg{res: panevis.ShowRef(t.Team, t.Name, t.Socket, t.PaneID)} }
}

func addVendorCmd(req userops.AddRequest) tea.Cmd {
	return func() tea.Msg {
		_, err := userops.Add(req)
		return opDoneMsg{verb: "add", name: req.Name, err: err}
	}
}

func editVendorCmd(req userops.EditRequest) tea.Cmd {
	return func() tea.Msg {
		_, err := userops.Edit(req)
		return opDoneMsg{verb: "edit", name: req.Name, err: err}
	}
}

func removeVendorCmd(name string) tea.Cmd {
	return func() tea.Msg {
		_, err := userops.Remove(userops.RemoveRequest{Name: name})
		return opDoneMsg{verb: "remove", name: name, err: err}
	}
}

// modelsMsg carries the result of fetching a vendor's model list for the picker.
// It implements screenOwnedAsyncMsg so a result arriving after the user has
// left the picker is dropped — otherwise a stale modelList would leak into the
// next picker visit.
type modelsMsg struct {
	models []models.Model
	err    error
}

func (modelsMsg) owningScreen() screen { return screenModelPick }

// modelsFetchTimeout backstops the picker fetch. models.FetchWithKey caps its
// own HTTP client too; this outer ceiling guarantees a hung dial can't wedge
// the picker in its loading state forever.
const modelsFetchTimeout = 12 * time.Second

// fetchModelsCmd fetches the vendor's model list off the Update goroutine and
// reuses models.FetchWithKey (the same secrets-free core the spawn probe uses).
// For an add the key is the one just typed into the form (not yet persisted);
// for an edit it's read from disk via secrets.Keyget. Any error / empty result
// is delivered as a modelsMsg and the picker falls back to manual text entry.
func fetchModelsCmd(mode formMode, name, endpoint, apiKey string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), modelsFetchTimeout)
		defer cancel()
		var key []byte
		if mode == modeAdd {
			key = []byte(apiKey)
		} else {
			key, _ = secrets.Keyget(name) // best-effort; empty key still attempts
		}
		list, err := models.FetchWithKey(ctx, endpoint, key)
		return modelsMsg{models: list, err: err}
	}
}

// keysetMsg carries the loaded key set + the vendor's current rotation strategy
// when entering the key manager (or reloading after a change). Owned by
// screenKeys.
type keysetMsg struct {
	keys     []secrets.KeyEntry
	rotation string
	err      error
}

func (keysetMsg) owningScreen() screen { return screenKeys }

// keysSavedMsg reports the outcome of a SaveKeySet write (toggle/add/edit/delete).
// Owned by screenKeys.
type keysSavedMsg struct{ err error }

func (keysSavedMsg) owningScreen() screen { return screenKeys }

// rotationSetMsg reports the outcome of cycling the rotation strategy. Owned
// by screenKeys.
type rotationSetMsg struct {
	rotation string
	err      error
}

func (rotationSetMsg) owningScreen() screen { return screenKeys }

// loadKeysetCmd reads the vendor's key set (LoadKeySet) and its current
// key_rotation (from vendors.toml) off the Update goroutine. A config.Load
// failure surfaces into keysetMsg.err so a corrupt vendors.toml is visible in
// the key manager instead of silently leaving rotation empty; the LoadKeySet
// error (different on-disk file) takes precedence. Either error fails the load.
func loadKeysetCmd(vendor string) tea.Cmd {
	return func() tea.Msg {
		ks, err := secrets.LoadKeySet(vendor)
		rotation := ""
		cfg, cErr := config.Load()
		if cErr != nil {
			// Take the LoadKeySet error if there is one; otherwise surface the
			// config.Load error so the user sees the corrupt vendors.toml.
			if err == nil {
				err = fmt.Errorf("load vendors.toml: %w", cErr)
			}
		} else if v, ok := cfg.Vendors[vendor]; ok {
			rotation = v.KeyRotation
		}
		return keysetMsg{keys: ks, rotation: rotation, err: err}
	}
}

// saveKeysetCmd persists a snapshot of the current key set. The snapshot is
// copied so a later in-memory mutation can't change what this write commits.
func (m Model) saveKeysetCmd() tea.Cmd {
	vendor := m.keyVendor
	snapshot := cloneKeys(m.keys)
	return func() tea.Msg {
		return keysSavedMsg{err: secrets.SaveKeySet(vendor, snapshot)}
	}
}

// setRotationCmd applies a new rotation strategy via userops.Edit.
func setRotationCmd(vendor, next string) tea.Cmd {
	return func() tea.Msg {
		_, err := userops.Edit(userops.EditRequest{Name: vendor, KeyRotation: &next})
		return rotationSetMsg{rotation: next, err: err}
	}
}

// cloneKeys returns a shallow copy of a key set (entries are value types).
func cloneKeys(ks []secrets.KeyEntry) []secrets.KeyEntry {
	out := make([]secrets.KeyEntry, len(ks))
	copy(out, ks)
	return out
}

// nextRotation cycles off → round_robin → random → off (empty == off). Routed
// through config.ParseKeyRotation so an unrecognized value resets to off
// explicitly rather than silently advancing to round_robin via off.Next().
func nextRotation(cur string) string {
	r, err := config.ParseKeyRotation(cur)
	if err != nil {
		// Invalid input: reset to off (safe default; cycle resumes from off).
		return string(config.RotationOff)
	}
	return string(r.Next())
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

// Update is the single tea.Model entry point. Async results (vendorsMsg etc.)
// are handled regardless of screen unless they implement screenOwnedAsyncMsg —
// in that case Update drops the message when the user has navigated off the
// owning screen, so e.g. a slow models-fetch result can't reach the vendor list
// after the user esc'd back. Key handling dispatches on the active screen.
// ctrl+c always quits.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if owned, ok := msg.(screenOwnedAsyncMsg); ok {
		if owned.owningScreen() != m.screen {
			return m, nil
		}
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case vendorsMsg:
		m.loading = false
		m.vendors = msg.vendors
		m.vendorsErr = msg.err
		// The cursor may also rest on the trailing "+ Add vendor…" row at index
		// len(vendors); clamp to that, not len-1.
		if m.vendorCursor > len(m.vendors) {
			m.vendorCursor = len(m.vendors)
		}
		return m, nil

	case boardMsg:
		// Even when the owner-check accepts the message (screen == screenSpawn),
		// a stale refresh from a PRIOR board visit must still be dropped: the
		// epoch is bumped on each board entry, so a discover scheduled before
		// re-entry has msg.epoch < m.boardEpoch and must NOT clobber the fresh
		// visit's loading=true / teammates list.
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		m.loading = false
		// Group rows by team so each team's members render contiguously under one
		// header. spawnCursor stays a FLAT teammate index; sorting here (every
		// board update path, including tests) lets the view assume grouping.
		m.teammates = groupByTeam(msg.teammates)
		m.spawnErr = msg.teamErr
		m.jobs = msg.jobs
		m.sessionTitles = msg.sessionTitles
		// Keep the teammate cursor in range as the row count changes.
		if m.spawnCursor >= len(m.teammates) {
			m.spawnCursor = len(m.teammates) - 1
		}
		if m.spawnCursor < 0 {
			m.spawnCursor = 0
		}
		return m, nil

	case paneVisMsg:
		// Surface the hide/show outcome on the board's status line, then reload
		// so the HIDDEN column reflects the new state. boardMsg does NOT touch
		// boardStatus, so the message survives the immediate refresh.
		r := msg.res
		if r.OK {
			m.boardStatusErr = false
			m.boardStatus = fmt.Sprintf("%s %s: ok", r.Action, r.Name)
		} else {
			m.boardStatusErr = true
			m.boardStatus = fmt.Sprintf("%s %s failed: %s %s", r.Action, r.Name, r.ErrorCode, r.ErrorMsg)
			if r.Suggestion != "" {
				m.boardStatus += " — " + r.Suggestion
			}
		}
		return m, loadBoard(m.boardEpoch)

	case boardTickMsg:
		// Only the current tick chain, and only while the board is open, keeps
		// refreshing — a stale or off-board tick stops the chain.
		if m.screen == screenSpawn && msg.epoch == m.boardEpoch {
			return m, tea.Batch(loadBoard(msg.epoch), boardTick(msg.epoch))
		}
		return m, nil

	case workflowsMsg:
		// A stale refresh from a prior Workflows visit (msg.epoch < the bumped
		// epoch) must not clobber the fresh visit's state — mirror boardMsg.
		if msg.epoch != m.workflowsEpoch {
			return m, nil
		}
		m.loading = false
		m.workflowJobs = msg.jobs
		m.workflowRuns = msg.runs
		m.workflowsErr = msg.err
		m.wfActivity = msg.activity
		// Only a resolve load carries titles; a live tick leaves the last-resolved map in place so an
		// out-of-order tick can't clobber a fresh resolve back to a pre-resolve snapshot.
		if msg.titlesResolved {
			m.sessionTitles = msg.sessionTitles
		}
		// Re-derive focus on a run-set change (a GC'd focused run, the 0/1/>1 routing) and
		// clamp the run/phase/agent cursors to the new data.
		m.rerootWorkflows(false)
		return m, nil

	case workflowCtlMsg:
		// A stale result from a prior Workflows visit must not mutate a fresh one —
		// mirror workflowsMsg's epoch gate.
		if msg.epoch != m.workflowsEpoch {
			return m, nil
		}
		// Surface the control outcome on the board status line, then reload so the run's status
		// reflects the new state (mirror paneVisMsg). workflowsMsg does NOT touch workflowStatus, so
		// the message survives the refresh. A successful restart clears the line instead — the leaf
		// flips to a Running dot on the reload, the natural in-place feedback. The run id + error are
		// opaque/operator-supplied text, so scrub them like the rest of the board before display.
		runID := shortRunID(sessiontitle.CleanTitle(msg.runID))
		if msg.err != nil {
			m.workflowStatusErr = true
			m.workflowStatus = fmt.Sprintf("%s %s failed: %s", msg.verb, runID,
				sessiontitle.CleanTitle(msg.err.Error()))
		} else if msg.verb == "restart" {
			// A successful restart needs no standalone confirmation — the leaf flips to a ● Running
			// dot on the reload below, which is the natural in-place feedback; clear any stale line.
			m.workflowStatusErr = false
			m.workflowStatus = ""
		} else {
			m.workflowStatusErr = false
			m.workflowStatus = fmt.Sprintf("%s %s: ok", msg.verb, runID)
		}
		return m, loadWorkflows(m.workflowsEpoch, true)

	case wfDetailMsg:
		// Drop a read that answers a prior focused leaf (a slow leaf-A read landing after the
		// user moved to leaf-B) so the inline detail never shows the wrong leaf's answer.
		if msg.nonce != m.wfDetailNonce {
			return m, nil
		}
		m.wfDetailJob = msg.job
		m.wfDetailPrompt = msg.prompt
		m.wfDetailAnswer = msg.answer
		m.wfDetailIO = msg.present
		return m, nil

	case workflowsTickMsg:
		if m.screen == screenWorkflows && msg.epoch == m.workflowsEpoch {
			return m, tea.Batch(loadWorkflows(msg.epoch, false), workflowsTick(msg.epoch, m.workflowsTickInterval()))
		}
		return m, nil

	case modelsMsg:
		m.loading = false
		m.modelList = msg.models
		m.modelsErr = msg.err
		m.modelCursor = 0
		return m, nil

	case opDoneMsg:
		m.loading = false
		m.screen = screenResult
		if msg.err != nil {
			m.resultErr = true
			m.result = fmt.Sprintf("%s %q failed:\n\n%v", msg.verb, msg.name, msg.err)
		} else {
			m.resultErr = false
			m.result = fmt.Sprintf("%s %q: OK", msg.verb, msg.name)
		}
		return m, nil

	case keysetMsg:
		if msg.err != nil {
			m.keyErr = msg.err.Error()
			m.keys = nil
		} else {
			m.keys = msg.keys
			m.keyRotation = msg.rotation
			m.keyErr = ""
		}
		if m.keyCursor > len(m.keys) {
			m.keyCursor = len(m.keys)
		}
		if m.keyCursor < 0 {
			m.keyCursor = 0
		}
		return m, nil

	case keysSavedMsg:
		// On a save failure the in-memory m.keys reflects the attempted mutation
		// but the on-disk keys.json still holds the previous state — keyget would
		// keep handing out the old keys. Surface the error AND reload the on-disk
		// truth so the UI no longer disagrees with what apiKeyHelper will read.
		if msg.err != nil {
			m.keyErr = "save failed: " + msg.err.Error()
			return m, loadKeysetCmd(m.keyVendor)
		}
		m.keyErr = ""
		return m, nil

	case rotationSetMsg:
		if msg.err != nil {
			m.keyErr = msg.err.Error()
		} else {
			m.keyRotation = msg.rotation
			m.keyErr = ""
		}
		return m, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			m.quitting = true
			return m, tea.Quit
		}
		switch m.screen {
		case screenList:
			return m.updateList(msg)
		case screenSpawn:
			return m.updateSpawn(msg)
		case screenWorkflows:
			return m.updateWorkflows(msg)
		case screenPickTemplate:
			return m.updatePickTemplate(msg)
		case screenForm:
			return m.updateForm(msg)
		case screenModelPick:
			return m.updateModelPick(msg)
		case screenRemoveConfirm:
			return m.updateRemoveConfirm(msg)
		case screenResult:
			return m.updateResult(msg)
		case screenKeys:
			return m.updateKeys(msg)
		case screenTeammateDetail:
			return m.updateTeammateDetail(msg)
		case screenSetup:
			return m.updateSetup(msg)
		case screenSetupTmux:
			return m.updateSetupTmux(msg)
		}
	}
	return m, nil
}

// toList returns to the Vendors list (the hub) and reloads it — after an
// add/edit/remove the content changed, and a plain cancel just re-reads.
func (m Model) toList() (tea.Model, tea.Cmd) {
	m.screen = screenList
	m.loading = true
	return m, loadVendors
}

// updateList drives the Vendors hub. The cursor ranges over [0, len(vendors)];
// the final index is the synthetic "+ Add vendor…" row. enter edits the
// highlighted vendor (or opens the add wizard on the Add row); d deletes it
// (with a confirm); tab switches to Spawn status; q/esc quit.
func (m Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	addRow := len(m.vendors) // index of the trailing "+ Add vendor…" row
	switch msg.String() {
	case "q", "esc":
		m.quitting = true
		return m, tea.Quit
	case "tab":
		m.screen = screenSpawn
		m.loading = true
		m.spawnCursor = 0
		m.boardStatus = "" // clear any stale hide/show line from a prior visit
		// Bump the epoch so a tick still pending from a previous board visit
		// can't double the refresh rate; start a fresh load + tick chain. The
		// epoch is also stamped on boardMsg so a refresh scheduled BEFORE the
		// bump can't overwrite the new visit's state (its msg.epoch fails the
		// gate in the boardMsg handler).
		m.boardEpoch++
		return m, tea.Batch(loadBoard(m.boardEpoch), boardTick(m.boardEpoch))
	case "up", "k":
		if m.vendorCursor > 0 {
			m.vendorCursor--
		}
	case "down", "j":
		if m.vendorCursor < addRow {
			m.vendorCursor++
		}
	case "enter":
		if m.vendorCursor == addRow {
			m.screen = screenPickTemplate
			m.tmplCursor = 0
			return m, nil
		}
		v := m.vendors[m.vendorCursor]
		m.form = newEditForm(v)
		m.formMode = modeEdit
		m.editName = v.Name
		m.screen = screenForm
		return m, textinput.Blink
	case "d":
		if m.vendorCursor < addRow { // a vendor row, not the Add row
			m.removeName = m.vendors[m.vendorCursor].Name
			m.screen = screenRemoveConfirm
		}
	}
	return m, nil
}

// updateSpawn drives the agent-status board. ↑/↓ move the teammate cursor; h/s
// hide/show the selected teammate (no-op when the list is empty); r reloads;
// tab/esc return to the Vendors list; q quits. The auto-refresh tick chain runs
// independently (see boardTickMsg).
func (m Model) updateSpawn(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.spawnCursor > 0 {
			m.spawnCursor--
		}
	case "down", "j":
		if m.spawnCursor < len(m.teammates)-1 {
			m.spawnCursor++
		}
	case "h":
		if len(m.teammates) == 0 {
			return m, nil
		}
		// Pass the discovered Teammate row (with its live Socket + PaneID) so
		// HideRef can scope tmux ops to the right server and double-check the
		// pane id against config.
		return m, hideTeammateCmd(m.teammates[m.spawnCursor])
	case "s":
		if len(m.teammates) == 0 {
			return m, nil
		}
		return m, showTeammateCmd(m.teammates[m.spawnCursor])
	case "enter":
		// Open the full-field detail card for the selected teammate: lets the
		// operator read values the table truncates (vendor/model/detail).
		if len(m.teammates) == 0 {
			return m, nil
		}
		m.screen = screenTeammateDetail
		return m, nil
	case "r":
		m.loading = true
		return m, loadBoard(m.boardEpoch)
	case "tab":
		// Advance the 3-way cycle: Agent status → Workflows. Bump the epoch so a
		// tick still pending from a previous Workflows visit can't double the
		// refresh rate, and start a fresh load + tick chain (mirror updateList→spawn).
		return m.toWorkflows()
	case "esc":
		return m.toList()
	case "q":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

// toWorkflows enters the Workflows board: bump the epoch so a tick pending from a
// prior visit can't double the refresh rate, reset the per-visit status / activity /
// cursors, route to the run picker or a focused run (rerootWorkflows), and start a
// fresh load + tick chain (mirror updateList→spawn).
func (m Model) toWorkflows() (tea.Model, tea.Cmd) {
	m.screen = screenWorkflows
	m.loading = true
	m.workflowsEpoch++
	m.workflowStatus = ""
	m.wfActivity = nil
	m.wfRunCursor, m.wfPhaseCursor, m.wfAgentCursor = 0, 0, 0
	m.rerootWorkflows(true) // fresh 0/1/>1 routing on entry
	return m, tea.Batch(loadWorkflows(m.workflowsEpoch, true), workflowsTick(m.workflowsEpoch, m.workflowsTickInterval()))
}

// wfGroups is the board's run→phase→agent tree (newest-first) — the single source the picker, phases,
// agents, and all three cursors index.
func (m Model) wfGroups() []runGroup { return groupByRun(m.workflowJobs, m.workflowRuns) }

// focusedGroup returns the run the board is rooted on, ok=false when focusedRunID is unset or GC'd.
func (m Model) focusedGroup() (runGroup, bool) {
	for _, g := range m.wfGroups() {
		if g.runID == m.focusedRunID {
			return g, true
		}
	}
	return runGroup{}, false
}

// focusedPhase returns the phase under wfPhaseCursor in the focused run.
func (m Model) focusedPhase() (runPhaseGroup, bool) {
	g, ok := m.focusedGroup()
	if !ok || m.wfPhaseCursor < 0 || m.wfPhaseCursor >= len(g.phases) {
		return runPhaseGroup{}, false
	}
	return g.phases[m.wfPhaseCursor], true
}

// selectedLeaf returns the agent (job) under wfAgentCursor in the focused phase.
func (m Model) selectedLeaf() (subagent.Result, bool) {
	p, ok := m.focusedPhase()
	if !ok || m.wfAgentCursor < 0 || m.wfAgentCursor >= len(p.jobs) {
		return subagent.Result{}, false
	}
	return p.jobs[m.wfAgentCursor], true
}

// selectedRunID returns the focused run id (x/r/s act on the whole run, even when the focused phase
// has no agents).
func (m Model) selectedRunID() (string, bool) {
	if _, ok := m.focusedGroup(); ok {
		return m.focusedRunID, true
	}
	return "", false
}

// rerootWorkflows re-derives focus + clamps the cursors after the run set changes. On entry
// (initial=true) it routes by run count: 0 → empty Phases, 1 → auto-focus the sole run, >1 → the run
// picker. On a refresh (initial=false) it preserves the user's drill state unless the focused run was
// GC'd, then re-routes the same way.
func (m *Model) rerootWorkflows(initial bool) {
	groups := m.wfGroups()
	focusExists := false
	for _, g := range groups {
		if g.runID == m.focusedRunID {
			focusExists = true
			break
		}
	}
	if initial || !focusExists {
		switch {
		case len(groups) == 0:
			m.focusedRunID, m.wfMode = "", wfModePhases
		case len(groups) == 1:
			m.focusedRunID, m.wfMode = groups[0].runID, wfModePhases
		default:
			m.focusedRunID, m.wfMode = "", wfModePicker
		}
		m.wfPhaseCursor, m.wfAgentCursor = 0, 0
	}
	m.clampWfCursors()
}

// clampWfCursors bounds the run/phase/agent cursors to the live data and drops out of agent mode when
// the focused phase has no agents, so Enter/render can never index past the end.
func (m *Model) clampWfCursors() {
	groups := m.wfGroups()
	m.wfRunCursor = clampIndex(m.wfRunCursor, len(groups))
	g, ok := m.focusedGroup()
	if !ok {
		m.wfPhaseCursor, m.wfAgentCursor = 0, 0
		return
	}
	m.wfPhaseCursor = clampIndex(m.wfPhaseCursor, len(g.phases))
	agents := 0
	if m.wfPhaseCursor < len(g.phases) {
		agents = len(g.phases[m.wfPhaseCursor].jobs)
	}
	m.wfAgentCursor = clampIndex(m.wfAgentCursor, agents)
	if m.wfMode == wfModeAgent && agents == 0 {
		m.wfMode = wfModePhases
	}
}

// clampIndex keeps i in [0, n-1]; returns 0 when n==0 (an empty list parks the cursor at 0).
func clampIndex(i, n int) int {
	switch {
	case n <= 0:
		return 0
	case i >= n:
		return n - 1
	case i < 0:
		return 0
	default:
		return i
	}
}

// updateWorkflows drives the Workflows master-detail board, branching on wfMode (the run picker,
// the Phases overview, or an agent's detail). R reloads; tab returns to the Vendors list (closing
// the List → Agent status → Workflows → List cycle); q/ctrl+c quit. The per-mode handlers own ↑/↓,
// enter (descend), esc (ascend), and the run controls x/r/s. The auto-refresh tick chain runs
// independently (see workflowsTickMsg).
func (m Model) updateWorkflows(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.wfSaving {
		return m.updateWfSaveInput(msg)
	}
	if msg.String() != "d" && m.wfDeleteArm != "" {
		m.wfDeleteArm = "" // any non-d key disarms a pending delete
		if m.workflowStatus == deleteArmPrompt {
			m.workflowStatus = ""
		}
	}
	switch msg.String() {
	case "R", "ctrl+r":
		m.loading = true
		return m, loadWorkflows(m.workflowsEpoch, true)
	case "tab":
		return m.toList()
	case "q":
		m.quitting = true
		return m, tea.Quit
	}
	switch m.wfMode {
	case wfModePicker:
		return m.updateWfPicker(msg)
	case wfModeAgent:
		return m.updateWfAgent(msg)
	default:
		return m.updateWfPhases(msg)
	}
}

// updateWfPicker (run picker, >1 run): ↑/↓ choose a run, →/enter descend into it; esc/tab → Vendors
// (← clamps here — it never leaves the board).
func (m Model) updateWfPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	groups := m.wfGroups()
	switch msg.String() {
	case "up":
		m.wfRunCursor = clampIndex(m.wfRunCursor-1, len(groups))
	case "down":
		m.wfRunCursor = clampIndex(m.wfRunCursor+1, len(groups))
	case "right", "enter":
		return m.wfDescend()
	case "esc":
		return m.wfAscend(true)
	case "left":
		return m.wfAscend(false) // ← clamps at the picker (top level); esc/tab leave
	case "d":
		if m.wfRunCursor < len(groups) {
			return m.armOrDelete(groups[m.wfRunCursor].runID)
		}
	}
	return m, nil
}

// deleteArmPrompt is the status shown after the first `d` while a delete is armed.
const deleteArmPrompt = "press d again to delete this run · any other key cancels"

// armOrDelete is the two-press delete guard: the first `d` on a run arms it (sets the prompt); a second
// `d` on the SAME run confirms and dispatches the delete. (updateWorkflows disarms on any other key.)
func (m Model) armOrDelete(runID string) (tea.Model, tea.Cmd) {
	if m.wfDeleteArm == runID {
		m.wfDeleteArm = ""
		if m.workflowStatus == deleteArmPrompt {
			m.workflowStatus = "" // clear the prompt; the delete's own outcome replaces it
		}
		return m, deleteRunCmd(runID, m.workflowsEpoch)
	}
	m.wfDeleteArm = runID
	m.workflowStatusErr = false
	m.workflowStatus = deleteArmPrompt
	return m, nil
}

// updateWfPhases (L1): ↑/↓ walk phases, → descend into a non-empty phase's agents, ← ascend to the
// picker (multi-run) or clamp at the top (single-run); esc/tab → Vendors; x/r/s control the focused run.
func (m Model) updateWfPhases(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	g, ok := m.focusedGroup()
	switch msg.String() {
	case "up":
		if ok {
			m.wfPhaseCursor = clampIndex(m.wfPhaseCursor-1, len(g.phases))
			m.wfAgentCursor = 0
		}
	case "down":
		if ok {
			m.wfPhaseCursor = clampIndex(m.wfPhaseCursor+1, len(g.phases))
			m.wfAgentCursor = 0
		}
	case "right", "enter":
		return m.wfDescend()
	case "esc":
		return m.wfAscend(true)
	case "left":
		return m.wfAscend(false) // ← ascends to the picker (multi-run) or clamps (single-run); esc/tab leave
	default:
		return m.wfControl(msg)
	}
	return m, nil
}

// updateWfAgent (L2): ↑/↓ walk agents (reloading the inline detail), j/k scroll that detail, ← ascend
// to Phases; r restarts ONLY the focused agent; x/s control the focused run.
func (m Model) updateWfAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p, ok := m.focusedPhase()
	switch msg.String() {
	case "up":
		if ok {
			m.wfAgentCursor = clampIndex(m.wfAgentCursor-1, len(p.jobs))
			return m.focusLeafIO()
		}
	case "down":
		if ok {
			m.wfAgentCursor = clampIndex(m.wfAgentCursor+1, len(p.jobs))
			return m.focusLeafIO()
		}
	case "j":
		m.wfCardScroll = m.clampCardScroll(m.wfCardScroll + 1)
	case "k":
		m.wfCardScroll = m.clampCardScroll(m.wfCardScroll - 1)
	case "left", "esc":
		return m.wfAscend(true)
	case "right":
		return m, nil // already at the deepest level
	case "enter":
		// Fold/unfold the focused agent's prompt (collapsed by default); re-clamp the scroll
		// since the detail height changes.
		m.wfPromptExpanded = !m.wfPromptExpanded
		m.wfCardScroll = m.clampCardScroll(m.wfCardScroll)
		return m, nil
	case "r":
		return m.restartFocusedLeaf()
	default:
		return m.wfControl(msg)
	}
	return m, nil
}

// wfDescend drops one level: picker → Phases (focus the cursored run), Phases → agent detail (when
// the focused phase has agents, loading the first agent's io). The agent level is the deepest.
func (m Model) wfDescend() (tea.Model, tea.Cmd) {
	switch m.wfMode {
	case wfModePicker:
		groups := m.wfGroups()
		if m.wfRunCursor < len(groups) {
			m.focusedRunID = groups[m.wfRunCursor].runID
			m.wfMode = wfModePhases
			m.wfPhaseCursor, m.wfAgentCursor = 0, 0
		}
	case wfModePhases:
		if p, ok := m.focusedPhase(); ok && len(p.jobs) > 0 {
			m.wfMode = wfModeAgent
			m.wfAgentCursor = 0
			return m.focusLeafIO()
		}
	}
	return m, nil
}

// wfAscend climbs one level: agent → Phases, Phases → picker (multi-run). At the board's TOP level
// (the picker, or single-run Phases) there's nowhere to climb: exitAtTop leaves for Vendors (esc/tab)
// vs stays put (←), so repeated ← can't fall out of the board.
func (m Model) wfAscend(exitAtTop bool) (tea.Model, tea.Cmd) {
	switch m.wfMode {
	case wfModeAgent:
		m.wfMode = wfModePhases
		return m, nil
	case wfModePhases:
		if len(m.wfGroups()) > 1 {
			m.wfMode, m.focusedRunID = wfModePicker, ""
			return m, nil
		}
	}
	if exitAtTop {
		return m.toList()
	}
	return m, nil
}

// focusLeafIO resets the detail scroll and loads the focused leaf's prompt/answer into the inline
// detail pane (nonce-gated so a slow read for a prior leaf is dropped). Called whenever the agent
// cursor lands on a new leaf.
func (m Model) focusLeafIO() (tea.Model, tea.Cmd) {
	m.wfCardScroll = 0
	m.wfPromptExpanded = false // each newly focused leaf starts with its prompt collapsed
	job, ok := m.selectedLeaf()
	if !ok {
		m.wfDetailJob, m.wfDetailPrompt, m.wfDetailAnswer, m.wfDetailIO = subagent.Result{}, "", "", false
		return m, nil
	}
	m.wfDetailNonce++ // drop a slow read for a previously-focused leaf
	return m, loadLeafIOCmd(job, m.wfDetailNonce)
}

// clampCardScroll bounds the inline-detail scroll offset to [0, lines-viewport] so j/k never scroll
// past the content (and a stale offset from a longer leaf snaps back on a shorter one).
func (m Model) clampCardScroll(v int) int {
	max := len(m.agentDetailLines(m.wfAgentRightWidth())) - m.boardBodyHeight()
	if max < 0 {
		max = 0
	}
	switch {
	case v < 0:
		return 0
	case v > max:
		return max
	default:
		return v
	}
}

// restartFocusedLeaf restarts ONLY the focused agent — workflow.Restart drops its journal entry and
// resumes, re-running just this leaf (+ any downstream leaf whose input shifted). A leaf with no
// persisted JournalKey falls back to a whole-run restart so r is never a silent no-op.
func (m Model) restartFocusedLeaf() (tea.Model, tea.Cmd) {
	job, ok := m.selectedLeaf()
	if !ok {
		return m, nil
	}
	runID, rok := m.selectedRunID()
	if !rok {
		return m, nil
	}
	return m, restartCmd(runID, job.JournalKey, m.workflowsEpoch)
}

// wfControl runs the run-level controls (x stop / r restart / s save) shared by the Phases pane and
// the Agent pane's non-r keys — all targeting the FOCUSED run, so they work even when the focused
// phase has no agents. s opens a name prompt and saves the focused run as a named, reusable workflow
// (<ConfigDir>/workflows/<name>.star + .json).
func (m Model) wfControl(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	runID, ok := m.selectedRunID()
	if !ok {
		return m, nil
	}
	switch msg.String() {
	case "x":
		return m, stopRunCmd(runID, m.workflowsEpoch)
	case "r":
		return m, restartCmd(runID, "", m.workflowsEpoch) // whole run (no single leaf focused)
	case "d":
		return m.armOrDelete(runID)
	case "s":
		return m.startSaveWorkflow()
	}
	return m, nil
}

// startSaveWorkflow opens the name input to save the focused run as a named, reusable workflow
// (prefilled with the run's name). updateWfSaveInput then handles enter (save) / esc (cancel).
func (m Model) startSaveWorkflow() (tea.Model, tea.Cmd) {
	g, ok := m.focusedGroup()
	if !ok {
		return m, nil
	}
	m.wfSaving = true
	m.wfSaveInput = newTextInput(g.name, "workflow name", false)
	m.wfSaveInput.Focus()
	return m, textinput.Blink
}

// updateWfSaveInput drives the save-workflow name prompt: enter saves the focused run under the typed
// name (a blank name or no focused run just cancels), esc cancels, any other key edits the input.
func (m Model) updateWfSaveInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.wfSaving = false
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.wfSaveInput.Value())
		runID, ok := m.selectedRunID()
		m.wfSaving = false
		if !ok || name == "" {
			return m, nil
		}
		g, _ := m.focusedGroup()
		return m, saveWorkflowCmd(runID, name, g.sessionID, g.description, m.workflowsEpoch)
	}
	var cmd tea.Cmd
	m.wfSaveInput, cmd = m.wfSaveInput.Update(msg)
	return m, cmd
}

// saveWorkflowCmd saves a run as a named workflow off the Update goroutine; the outcome rides the
// shared workflowCtlMsg (verb "save"), epoch-gated like the others.
func saveWorkflowCmd(runID, name, sessionID, description string, epoch int) tea.Cmd {
	return func() tea.Msg {
		err := subagent.SaveWorkflow(runID, name, sessionID, description)
		return workflowCtlMsg{verb: "save", runID: runID, err: err, epoch: epoch}
	}
}

// updateTeammateDetail drives the read-only teammate detail card (board → enter).
// esc/enter/tab return to the board (cursor + data preserved, no reload); q
// quits. The card has no actions of its own — h/s still live on the board.
func (m Model) updateTeammateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		m.quitting = true
		return m, tea.Quit
	case "esc", "enter", "tab":
		m.screen = screenSpawn
	}
	return m, nil
}

func (m Model) updatePickTemplate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(Templates) + 1 // + synthetic "Custom" row
	switch msg.String() {
	case "esc", "q":
		return m.toList()
	case "up", "k":
		if m.tmplCursor > 0 {
			m.tmplCursor--
		}
	case "down", "j":
		if m.tmplCursor < n-1 {
			m.tmplCursor++
		}
	case "enter":
		var t Template // zero value = Custom (blank fields)
		if m.tmplCursor < len(Templates) {
			t = Templates[m.tmplCursor]
		}
		m.form = newAddForm(t)
		m.formMode = modeAdd
		m.screen = screenForm
		return m, textinput.Blink
	}
	return m, nil
}

func (m Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		return m.toList()
	}
	// Enter on the "Manage API keys →" action row (edit form only) opens the
	// per-vendor key manager and loads its key set.
	if msg.String() == "enter" && m.formMode == modeEdit && m.form.focusedKey() == "manage_keys" {
		m.screen = screenKeys
		m.keyVendor = m.editName
		m.keyCursor = 0
		m.keyEditing = false
		m.keyErr = ""
		m.keys = nil
		return m, loadKeysetCmd(m.editName)
	}
	// Enter on the Default model field opens the model picker instead of
	// advancing ("pick, don't type"). It requires a models_endpoint to hit;
	// custom vendors without one fall through to manual text entry.
	if msg.String() == "enter" && m.form.focusedKey() == "default_model" &&
		m.form.value("models_endpoint") != "" {
		m.screen = screenModelPick
		m.loading = true
		m.modelList = nil
		m.modelsErr = nil
		m.modelCursor = 0
		m.modelFilter = ""
		return m, fetchModelsCmd(m.formMode, m.editName,
			m.form.value("models_endpoint"), m.form.value("api_key"))
	}
	var cmd tea.Cmd
	var submitted bool
	m.form, cmd, submitted = m.form.Update(msg)
	if !submitted {
		return m, cmd
	}
	if m.formMode == modeAdd {
		return m.submitAdd()
	}
	return m.submitEdit()
}

// updateModelPick drives the model picker. Enter accepts the highlighted model
// id into the form's default_model field; esc (or an empty / failed fetch)
// returns to the form so the user can type the id manually — the required
// fallback when the vendor list is unavailable. Printable input narrows the
// list (type-to-filter), so vim j/k no longer navigate — letters are filter
// input and the arrow keys move the cursor.
func (m Model) updateModelPick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredModels()
	switch msg.String() {
	case "esc":
		m.screen = screenForm
		return m, textinput.Blink
	case "enter":
		if len(filtered) > 0 {
			m.form.setValue("default_model", filtered[m.modelCursor].ID)
		}
		m.screen = screenForm
		return m, textinput.Blink
	case "up":
		if m.modelCursor > 0 {
			m.modelCursor--
		}
	case "down":
		if m.modelCursor < len(filtered)-1 {
			m.modelCursor++
		}
	case "backspace", "ctrl+h": // some terminals report Backspace as Ctrl-H
		if m.modelFilter != "" {
			r := []rune(m.modelFilter)
			m.modelFilter = string(r[:len(r)-1])
			m.modelCursor = 0
		}
	default:
		if msg.Type == tea.KeyRunes && len(m.modelList) > 0 {
			m.modelFilter += string(msg.Runes)
			m.modelCursor = 0
		}
	}
	return m, nil
}

// filteredModels returns the models whose id contains modelFilter
// (case-insensitive substring — covers prefix, suffix, and infix). An empty
// filter returns the full list. modelCursor indexes into this result.
func (m Model) filteredModels() []models.Model {
	if m.modelFilter == "" {
		return m.modelList
	}
	q := strings.ToLower(m.modelFilter)
	var out []models.Model
	for _, mod := range m.modelList {
		if strings.Contains(strings.ToLower(mod.ID), q) {
			out = append(out, mod)
		}
	}
	return out
}

func (m Model) updateRemoveConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.loading = true
		return m, removeVendorCmd(m.removeName)
	case "n", "N", "esc", "q":
		return m.toList()
	}
	return m, nil
}

// updateResult returns to the Vendors list on any key press.
func (m Model) updateResult(_ tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.toList()
}

// setupOptionCount is the number of choices on the agent-teams setup screen
// (enable / already-set-up / not-now).
const setupOptionCount = 3

// updateSetup drives the first-run agent-teams setup nudge. Whatever the user
// picks, the choice is recorded (ackAgentTeams) so the screen never shows
// again. "enable it for me" writes ~/.claude/settings.json and leaves a restart
// note; the other two just dismiss. Once a note is showing, any key continues
// to the hub.
func (m Model) updateSetup(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.setupMsg != "" {
		return m.toList()
	}
	switch msg.String() {
	case "up", "k":
		if m.setupCursor > 0 {
			m.setupCursor--
		}
	case "down", "j":
		if m.setupCursor < setupOptionCount-1 {
			m.setupCursor++
		}
	case "enter":
		ackAgentTeams()
		if m.setupCursor == 0 { // "enable it for me"
			already, err := onboarding.EnableAgentTeams()
			switch {
			case err != nil:
				m.setupMsg = "couldn't write settings.json: " + err.Error()
			case already:
				m.setupMsg = "already set in settings.json — restart claude to take effect"
			default:
				m.setupMsg = "enabled in ~/.claude/settings.json — restart claude to take effect"
			}
			return m, nil
		}
		return m.toList() // "I've set it up myself" / "not now"
	case "esc", "q":
		ackAgentTeams()
		return m.toList()
	}
	return m, nil
}

// ackAgentTeams records that the user dealt with the setup nudge so it never
// shows again. Best-effort: a save failure just means it may reappear next run.
func ackAgentTeams() {
	st, _ := onboarding.LoadState()
	st.AgentTeamsAck = true
	_ = st.Save()
}

// tmuxOptionCount is the number of choices on the tmux setup screen
// (install / skip-subagent-only).
const tmuxOptionCount = 2

// updateSetupTmux drives the first-run tmux setup nudge. "install it" quits ccf
// and leaves the install command on screen (postQuitNote) — it does NOT ack, so
// the nudge returns until tmux is actually present. "skip — subagent mode only"
// records TmuxAck so we never nudge again, then proceeds to the agent-teams
// screen (if needed) or the hub.
func (m Model) updateSetupTmux(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.tmuxCursor > 0 {
			m.tmuxCursor--
		}
	case "down", "j":
		if m.tmuxCursor < tmuxOptionCount-1 {
			m.tmuxCursor++
		}
	case "enter":
		if m.tmuxCursor == 0 { // "install it" → quit so the user can run the command
			m.quitting = true
			m.postQuitNote = tmuxInstallNote()
			return m, tea.Quit
		}
		return m.skipTmux() // "skip — I'll only use subagent mode"
	case "esc", "q":
		return m.skipTmux()
	}
	return m, nil
}

// skipTmux records the "subagent mode only" choice (TmuxAck) and advances to the
// agent-teams screen if that nudge is still needed, else the hub.
func (m Model) skipTmux() (tea.Model, tea.Cmd) {
	st, _ := onboarding.LoadState()
	st.TmuxAck = true
	_ = st.Save()
	if onboarding.NeedsAgentTeamsSetup() {
		m.screen = screenSetup
		return m, nil
	}
	return m.toList()
}

// tmuxInstallNote is printed by tui.Run AFTER the program exits when the user
// chose "install it" — the OS-appropriate command + a restart reminder. It is
// printed outside the TUI so it survives the screen teardown.
func tmuxInstallNote() string {
	return "Install tmux, then run ccf again:\n\n    " + onboarding.TmuxInstallHint() + "\n"
}

// updateKeys drives the key manager. While the password input is active
// (keyEditing) keystrokes edit the new key value; otherwise the cursor walks
// the key rows + the trailing "+ Add key…" row and the action keys mutate the
// set. esc returns to the EDIT form.
func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.keyEditing {
		return m.updateKeyInput(msg)
	}
	addRow := len(m.keys) // index of the synthetic "+ Add key…" row
	switch msg.String() {
	case "esc":
		m.screen = screenForm
		m.keyErr = ""
		return m, textinput.Blink
	case "up", "k":
		if m.keyCursor > 0 {
			m.keyCursor--
		}
	case "down", "j":
		if m.keyCursor < addRow {
			m.keyCursor++
		}
	case "t":
		return m, setRotationCmd(m.keyVendor, nextRotation(m.keyRotation))
	case "a":
		return m.startAddKey()
	case "enter":
		if m.keyCursor == addRow {
			return m.startAddKey()
		}
		return m.startEditKey(m.keyCursor)
	case "e":
		if m.keyCursor < addRow {
			return m.startEditKey(m.keyCursor)
		}
	case " ", "space":
		if m.keyCursor < addRow {
			m.keys[m.keyCursor].Enabled = !m.keys[m.keyCursor].Enabled
			return m, m.saveKeysetCmd()
		}
	case "d":
		if m.keyCursor < addRow {
			m.keys = append(m.keys[:m.keyCursor], m.keys[m.keyCursor+1:]...)
			if m.keyCursor > len(m.keys) {
				m.keyCursor = len(m.keys)
			}
			return m, m.saveKeysetCmd()
		}
	}
	return m, nil
}

// updateKeyInput handles the add/edit password input. enter commits a non-empty
// value (append for add, replace for edit) and saves; esc cancels back to the
// list without changes. The typed value is never rendered in plaintext.
func (m Model) updateKeyInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.keyEditing = false
		m.keyErr = ""
		return m, nil
	case "enter":
		val := strings.TrimSpace(m.keyInput.Value())
		if val == "" {
			m.keyErr = "key cannot be empty"
			return m, nil
		}
		if m.keyEditIdx < 0 {
			// Add (the first add on a legacy vendor triggers the migration when
			// SaveKeySet writes keys.json — keys[0] was seeded from the legacy key).
			m.keys = append(m.keys, secrets.KeyEntry{Key: val, Enabled: true})
			m.keyCursor = len(m.keys) - 1
		} else if m.keyEditIdx < len(m.keys) {
			m.keys[m.keyEditIdx].Key = val
		}
		m.keyEditing = false
		m.keyErr = ""
		return m, m.saveKeysetCmd()
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return m, cmd
}

// startAddKey opens the password input to append a new key.
func (m Model) startAddKey() (tea.Model, tea.Cmd) {
	m.keyEditIdx = -1
	m.keyEditing = true
	m.keyErr = ""
	m.keyInput = newTextInput("", "new API key (stored 0600)", true)
	m.keyInput.Focus()
	return m, textinput.Blink
}

// startEditKey opens the password input to replace the value of key idx.
func (m Model) startEditKey(idx int) (tea.Model, tea.Cmd) {
	m.keyEditIdx = idx
	m.keyEditing = true
	m.keyErr = ""
	m.keyInput = newTextInput("", "new value for "+m.keyLabel(idx)+" (stored 0600)", true)
	m.keyInput.Focus()
	return m, textinput.Blink
}

// keyLabel returns the display label for key idx: its label, or "keyN" (1-based)
// when the label is empty. Never the key itself.
func (m Model) keyLabel(idx int) string {
	if idx < 0 || idx >= len(m.keys) {
		return ""
	}
	if l := strings.TrimSpace(m.keys[idx].Label); l != "" {
		return l
	}
	return fmt.Sprintf("key%d", idx+1)
}

// submitAdd validates the add form and dispatches userops.Add. Required-field
// gaps are surfaced inline (no command) so the user can fix them in place;
// vendor-side errors (bad key, unreachable) come back via opDoneMsg.
func (m Model) submitAdd() (tea.Model, tea.Cmd) {
	name := m.form.value("name")
	baseURL := m.form.value("base_url")
	modelsEndpoint := m.form.value("models_endpoint")
	apiKey := m.form.value("api_key")
	defaultModel := m.form.value("default_model")

	if missing := missingLabels(map[string]string{
		"Name":            name,
		"Base URL":        baseURL,
		"Models endpoint": modelsEndpoint,
		"API key":         apiKey,
		"Default model":   defaultModel,
	}, []string{"Name", "Base URL", "Models endpoint", "API key", "Default model"}); len(missing) > 0 {
		m.form.err = "required: " + strings.Join(missing, ", ")
		return m, nil
	}

	m.form.err = ""
	m.loading = true
	return m, addVendorCmd(userops.AddRequest{
		Name:           name,
		BaseURL:        baseURL,
		ModelsEndpoint: modelsEndpoint,
		DefaultModel:   defaultModel,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		APIKey:         apiKey,
		Enabled:        true,
	})
}

// submitEdit validates the edit form and dispatches userops.Edit.
func (m Model) submitEdit() (tea.Model, tea.Cmd) {
	baseURL := m.form.value("base_url")
	modelsEndpoint := m.form.value("models_endpoint")
	defaultModel := m.form.value("default_model")
	enabled := m.form.boolValue("enabled")

	if missing := missingLabels(map[string]string{
		"Base URL":        baseURL,
		"Models endpoint": modelsEndpoint,
		"Default model":   defaultModel,
	}, []string{"Base URL", "Models endpoint", "Default model"}); len(missing) > 0 {
		m.form.err = "required: " + strings.Join(missing, ", ")
		return m, nil
	}

	m.form.err = ""
	m.loading = true
	return m, editVendorCmd(userops.EditRequest{
		Name:           m.editName,
		BaseURL:        &baseURL,
		ModelsEndpoint: &modelsEndpoint,
		DefaultModel:   &defaultModel,
		Enabled:        &enabled,
	})
}

// missingLabels returns the labels (in the given order) whose value is empty.
func missingLabels(values map[string]string, order []string) []string {
	var missing []string
	for _, label := range order {
		if strings.TrimSpace(values[label]) == "" {
			missing = append(missing, label)
		}
	}
	return missing
}

// Run starts the bubbletea program against stdin/stdout. The caller is
// responsible for ensuring those are a terminal (see cmd/cc-fleet/tui.go).
func Run() error {
	final, err := tea.NewProgram(NewModel()).Run()
	if err != nil {
		return err
	}
	// The tmux setup screen's "install it" choice leaves a note to print AFTER
	// the program exits (so it survives the TUI teardown). bubbletea returns the
	// final model; read the note off it.
	if m, ok := final.(Model); ok && m.postQuitNote != "" {
		fmt.Println(m.postQuitNote)
	}
	return nil
}
