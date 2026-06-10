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
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/onboarding"
	"github.com/ethanhq/cc-fleet/internal/panevis"
	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teamhist"
	"github.com/ethanhq/cc-fleet/internal/teardown"
	"github.com/ethanhq/cc-fleet/internal/userops"
	"github.com/ethanhq/cc-fleet/internal/workflow"
)

// screen enumerates the TUI's views; one Model dispatches Update/View on the
// active screen.
type screen int

const (
	screenList screen = iota // the home/hub: Model Providers list + inline "+ Add" row
	screenSpawn
	screenPickTemplate
	screenForm
	screenModelPick
	screenKeys      // EDIT form → "Manage API keys →": per-vendor multi-key manager
	screenSetupTmux // first-run tmux setup nudge; shown before agent-teams/hub
	screenSetup     // first-run agent-teams setup nudge; shown before the hub
	screenCodexAuth // CLI-auth → codex: committing → consent → device-code login, modal-rendered
)

// addCategory is the provider class a picker row belongs to.
type addCategory int

const (
	addCatAnthropic addCategory = iota // Anthropic-native API (today's templates)
	addCatOpenAI                       // OpenAI-protocol API (chat / responses)
	addCatCLI                          // CLI auth (codex)
)

// codexAuthStage is the sub-state of screenCodexAuth.
type codexAuthStage int

const (
	codexAuthConsent      codexAuthStage = iota // risk notice; enter accepts
	codexAuthDevice                             // device-code shown; polling for authorization
	codexAuthChooseSource                       // a subscription is detected: reuse it or log in separately
	codexAuthCommitting                         // the add is committing; all keys trapped until opDoneMsg routes on
)

// formMode records whether the active form is an add or an edit so submit
// knows which userops call to make.
type formMode int

const (
	modeAdd formMode = iota
	modeEdit
)

// asMode is the active level of the master-detail Agents Board — all WITHIN
// screenSpawn, so the refresh/tick msg ownership stays on one screen.
// →/enter descend a level; ← ascends but CLAMPS at the board's top level; esc ascends too
// and only leaves for the Model Providers hub at the top — so the entity-level detail card
// always RETURNS on esc, never exits the board. Single-choice levels collapse: one project skips L0, one
// session skips L1 too.
type asMode int

const (
	asModeProjects  asMode = iota // L0: project rail | the cursored project's sessions (>1 project)
	asModeSessions                // L1: session rail | the cursored session's overview (>1 session)
	asModeBoxes                   // L2: the focused session's stacked boxes — Dynamic Workflows + Agent Teams + Subagents
	asModeEntity                  // L3: entity list | the focused entity's inline detail card (j/k scroll)
	asModeRunPhases               // run drill: the focused run's Phases | the selected phase's agents
	asModeRunAgent                // run drill: agent list | the selected agent's inline detail (j/k scroll)
)

// Model is the root bubbletea model.
type Model struct {
	screen screen
	width  int
	height int

	// Vendor data, loaded for the Model Providers list (the hub) and reused to seed the
	// edit form. vendorCursor ranges over [0, len(vendors)]; the final index is
	// the trailing "+ Add provider…" row.
	vendors      []userops.VendorView
	vendorsErr   error
	vendorCursor int

	// Add-wizard: the single grouped picker (cursor over the flat selectable rows)
	// and the protocol the chosen row implies (carried into submitAdd).
	tmplCursor  int
	addProtocol string

	// screenCodexAuth: an optional source choice (reuse a detected subscription, or
	// log in separately) then consent → device-code login. codexAuthEpoch tags each
	// login attempt so a prior visit's async msgs (esc then re-enter) drop.
	// codexPendingAdd holds the add request awaiting a source choice (cli-ride only).
	// A login-needed codex commits its row FIRST (so a concurrent remove's
	// referenced-check sees it), then logs in: codexCommittedName names that committed
	// row (the rollback target if the login is abandoned) and codexAddRef its
	// credential, so the login writes the file the provider's secret_ref names.
	codexAuthStage     codexAuthStage
	codexAuth          *codexproxy.LoginSession
	codexAuthErr       string
	codexAuthEpoch     int
	codexAuthCtx       context.Context    // device-login session scope; cancelled on abandon/success
	codexAuthCancel    context.CancelFunc // so an in-flight poll can't write a token after abandon
	codexPendingAdd    *userops.AddRequest
	codexAddRef        string
	codexCommittedName string // committed-but-not-yet-logged-in codex row; rolled back on abandon
	codexAuthAccount   string // detected cli-ride account, shown on the source-choice stage
	codexSourceCursor  int    // source-choice selection: 0 reuse the detected login, 1 log in separately

	// Agents Board (screenSpawn): live teammates, workflow runs, and async subagent
	// jobs in one master-detail board. asMode re-roots the levels (projects → sessions →
	// the session's Dynamic Workflows + Agent Teams + Subagents boxes → entity detail, and
	// the run drill below the boxes) WITHIN this one screen; focusedProject/focusedSessionID
	// root the deeper levels. boardEpoch
	// tags each auto-refresh tick chain so re-entering the board supersedes a stale
	// chain instead of stacking a second one. sessionMeta carries each session's
	// resolved /rename title + recorded working directory (the project grouping key).
	teammates   []teardown.Teammate
	spawnErr    error
	jobs        []subagent.Result
	sessionMeta map[string]sessiontitle.Meta
	// endedSeen maps an ended team (gone from live discovery, rendered from its
	// history record) to its record LastSeen, for the card's "last seen" line.
	endedSeen map[string]time.Time
	// confirm, when non-nil, is the centered confirm modal overlaying the board: every destructive
	// action (delete a run / ended team / job, clear a session's finished) opens one (see confirm.go).
	// pins is the pin-registry snapshot loaded each refresh — the board reads it to mark ★ rows and
	// to exclude pinned records from clear-finished.
	confirm    *confirmModal
	pins       pinned.Set
	boardEpoch int
	// boardSeen marks that a boardMsg has ever been accepted; a revisit then skips the
	// "discovering…" loading frame and keeps the previous frame until the fresh load
	// lands. boardEntryRoute defers the entry reset (cursors + focus park) to that
	// first accepted refresh, so the stale frame can still render.
	boardSeen        bool
	boardEntryRoute  bool
	asMode           asMode
	focusedProject   string
	focusedSessionID string
	asProjectCursor  int       // L0 rail row
	asSessionCursor  int       // L1 rail row
	asBoxCursor      int       // L2 continuum row: the session's runs, then its teams, then its jobs
	asEntitySrc      asRailRef // the collection L3 lists (set on descend from L2)
	asEntityCursor   int       // L3 entity row
	asCardScroll     int       // L3 detail-card scroll offset (j/k); reset on entity change
	// Focused-job inline detail (the L3 jobs collection): the job's prompt/answer io +
	// activity snapshot, read off the Update goroutine (the wf board's wfDetail* pattern).
	// asDetailJobID records WHICH job the loaded io belongs to, so a render only shows it
	// when it matches the focused job; asDetailNonce drops a slow read for a previously-
	// focused job; asPromptExpanded mirrors wfPromptExpanded.
	asDetailJobID    string
	asDetailPrompt   string
	asDetailAnswer   string
	asDetailIO       bool
	asDetailSnap     activitySnapshot
	asDetailNonce    int
	asDetailTerminal bool // the focused job was terminal when its io load was issued
	asPromptExpanded bool
	// Focused-teammate inline detail (the L3 team collection): the teammate's messages,
	// outputs, and token aggregates, read off the Update goroutine on the same nonce+epoch
	// gate the job card uses (asDetailNonce covers both detail kinds, so moving focus
	// between a job and a teammate invalidates the other's in-flight read). asMateKey
	// records WHICH teammate (mateKey) the loaded payload belongs to.
	asMateKey   string
	asMateSnap  teammateSnapshot
	asMateFound bool // transcript located (false renders the no-transcript note)
	// Cursored-team transcript aggregate for the session header (the L2 teams range):
	// summed member ↑ peak-context / ↓ output, loaded async on its own nonce gate.
	asTeamNonce int // own nonce: team stats and the job-card io load in the same batch must not invalidate each other
	asTeamKey   string
	asTeamCtx   int
	asTeamOut   int
	// boardJobsErr is the last refresh's jobs-scan failure, rendered on its OWN dim line under the
	// board — a persistent data-availability warning, not a one-shot outcome (those pop as modals).
	boardJobsErr error
	// wfLiveOn marks the 500ms workflow-only refresh chain as scheduled, so refreshes that
	// keep seeing running leaves don't stack a second chain.
	wfLiveOn bool

	// Run drill (screenSpawn, asModeRunPhases/asModeRunAgent): the master-detail levels below
	// a Dynamic Workflows box row. focusedRunID is the run being drilled; wfPhaseCursor and
	// wfAgentCursor index the focused run's phases and the focused phase's agents. wfActivity
	// holds each leaf's activity snapshot (read off the refresh goroutine, keyed by job id): a
	// running leaf's tokens climb live, and every leaf's tool count persists.
	workflowJobs  []subagent.Result
	workflowRuns  []subagent.WorkflowRun
	focusedRunID  string
	wfPhaseCursor int
	wfAgentCursor int
	wfActivity    map[string]activitySnapshot
	// wfRestarting is the per-run in-flight guard: a run id is added when a stop/restart/delete is
	// dispatched and removed when its workflowCtlMsg lands, so a second x/r/d on the same run is a
	// no-op until the first completes (and a restart shows a transient "restarting …" status meanwhile).
	wfRestarting map[string]bool
	// Save-workflow name prompt: `s` on a run row opens wfSaveInput (prefilled with the run name);
	// while wfSaving, keys route to the input (enter saves to ~/.config/cc-fleet/workflows/<name>.star,
	// esc cancels). wfSaveRun pins the TARGET run at open time — the board keeps refreshing under the
	// prompt and a reanchor could move the cursor, so enter must not re-resolve it from the row.
	wfSaveInput textinput.Model
	wfSaving    bool
	wfSaveRun   runGroup

	// Focused-agent inline detail (asModeRunAgent right pane): the focused leaf's prompt/answer
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
	// pickerTarget is the form field the model picker writes its choice back into
	// (default_model / strong_model / fast_model). Empty falls back to default_model.
	pickerTarget string

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

// NewModel returns the initial model. It normally parks on the Model Providers list
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

// boardMsg carries one Agents Board refresh: the discovered teammates
// (health + hidden annotated) and the async subagent jobs. It opts into
// screenOwnedAsyncMsg (owningScreen → screenSpawn) AND carries the boardEpoch
// that scheduled it, so a stale refresh from a prior board visit is dropped
// when the user re-enters (epoch++) or leaves the board.
type boardMsg struct {
	teammates   []teardown.Teammate
	teamErr     error
	jobs        []subagent.Result
	jobsErr     error
	runs        []subagent.WorkflowRun
	activity    map[string]activitySnapshot
	sessionMeta map[string]sessiontitle.Meta
	// endedSeen maps an ended team's name to its record LastSeen, so the card can
	// render "ended · last seen <ts>" without threading a time into the synthetic
	// Teammate. Empty when no team has ended.
	endedSeen map[string]time.Time
	pins      pinned.Set
	epoch     int
}

func (boardMsg) owningScreen() screen { return screenSpawn }

// boardTickMsg drives the board's auto-refresh. epoch identifies the tick chain
// that scheduled it; a tick whose epoch != Model.boardEpoch is stale (the user
// left and re-entered the board) and is dropped instead of rescheduling.
type boardTickMsg struct{ epoch int }

// boardRefreshInterval is the auto-refresh cadence while the board is open.
const boardRefreshInterval = 3 * time.Second

// workflowsLiveInterval is the tighter cadence the board's workflow refresh ticks at while a leaf is running,
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
// skips annotation (we can't enrich an empty list) and is fatal to the board; a
// jobs error degrades to no jobs and surfaces on its own line — the board never
// crashes on a data-source failure. The epoch carries through to boardMsg so
// Update can drop a stale refresh from a prior visit.
func loadBoard(epoch int) tea.Cmd {
	return func() tea.Msg {
		items, err := teardown.DiscoverTeammates()
		if err == nil {
			items = teardown.AnnotateHealth(items)
			items = teardown.AnnotateHidden(items)
			items = teardown.AnnotateLeadSession(items)
		}
		jobs, jobsErr := subagent.ListJobs()
		runs, activity, wfErr := loadWfData(jobs)
		if jobsErr == nil {
			jobsErr = wfErr
		}
		meta := sessiontitle.ResolveMeta(leadSessionIDs(items, jobs, runs))
		// Record live teams + synthesize ended ones AFTER annotation, so a pane
		// capture can never overwrite a synthetic row's `ended` status. Upsert runs
		// only on a successful discovery (else the live set is unknown) and is
		// best-effort — a record error never fails the board.
		if err == nil {
			_ = teamhist.Upsert(items, func(sessionID string) string { return meta[sessionID].Cwd })
		}
		ended, endedSeen := synthesizeEnded(items, meta)
		items = append(items, ended...)
		pins, _ := pinned.Snapshot() // best-effort: a read glitch just renders no ★ this tick
		return boardMsg{
			teammates:   items,
			teamErr:     err,
			jobs:        jobs,
			jobsErr:     jobsErr,
			runs:        runs,
			activity:    activity,
			sessionMeta: meta,
			endedSeen:   endedSeen,
			pins:        pins,
			epoch:       epoch,
		}
	}
}

// synthesizeEnded reads the team-history records and, for every recorded team
// absent from the live set, returns synthetic Status=="ended" teammates (PID 0,
// no PaneID/Socket — pane-dependent ops no-op on them) plus a team→LastSeen map
// for the card's "last seen" line. It also BACK-FILLS meta[leadSessionID].Cwd
// from a member record when the live resolution left it empty, so the card's
// transcript path resolves for an ended member. Best-effort: a List error yields
// no ended rows.
func synthesizeEnded(live []teardown.Teammate, meta map[string]sessiontitle.Meta) ([]teardown.Teammate, map[string]time.Time) {
	recs, err := teamhist.List()
	if err != nil || len(recs) == 0 {
		return nil, nil
	}
	liveTeams := map[string]struct{}{}
	for _, t := range live {
		liveTeams[t.Team] = struct{}{}
	}
	var ended []teardown.Teammate
	seen := map[string]time.Time{}
	for _, rec := range recs {
		if _, alive := liveTeams[rec.Team]; alive {
			continue // live discovery wins — the record is shadow data only
		}
		if ts, perr := time.Parse(time.RFC3339, rec.LastSeen); perr == nil {
			seen[rec.Team] = ts
		}
		for _, mr := range rec.Members {
			if mr.Cwd != "" && meta[mr.LeadSessionID].Cwd == "" {
				m := meta[mr.LeadSessionID]
				m.Cwd = mr.Cwd
				meta[mr.LeadSessionID] = m
			}
			ended = append(ended, teardown.Teammate{
				Name:          mr.Name,
				Team:          rec.Team,
				Vendor:        mr.Vendor,
				Model:         mr.Model,
				SpawnTime:     mr.SpawnTime,
				LeadSessionID: mr.LeadSessionID,
				Status:        endedStatus,
			})
		}
	}
	return ended, seen
}

// endedStatus marks a synthesized teammate row whose team is gone from live
// discovery: it renders faint with the word `ended`, h/s no-op on it, and it is
// excluded from the live "ok" / teammate-count rollups.
const endedStatus = "ended"

// loadWfData assembles the workflow half of a refresh from an already-listed job set: the
// run manifests plus each RunID-tagged leaf's activity sidecar (live tokens + tool calls).
func loadWfData(jobs []subagent.Result) ([]subagent.WorkflowRun, map[string]activitySnapshot, error) {
	activity := map[string]activitySnapshot{}
	for _, j := range jobs {
		if j.RunID == "" || j.JobID == "" {
			continue
		}
		if snap, ok := readLeafActivity(j.JobID); ok {
			activity[j.JobID] = snap
		}
	}
	runs, err := subagent.ListRuns()
	return runs, activity, err
}

// wfTagged filters a job list down to the RunID-tagged workflow leaves.
func wfTagged(jobs []subagent.Result) []subagent.Result {
	var out []subagent.Result
	for _, j := range jobs {
		if j.RunID != "" {
			out = append(out, j)
		}
	}
	return out
}

// wfRefreshMsg carries a LIGHT workflow-only refresh (run manifests + leaf jobs + activity
// sidecars — never teammate discovery or pane capture), driven by the 500ms live chain so a
// running run's counters climb smoothly between the 3s full refreshes. Owned by screenSpawn,
// boardEpoch-gated like boardMsg.
type wfRefreshMsg struct {
	jobs     []subagent.Result
	runs     []subagent.WorkflowRun
	activity map[string]activitySnapshot
	epoch    int
}

func (wfRefreshMsg) owningScreen() screen { return screenSpawn }

// wfLiveTickMsg drives the light chain; a stale epoch stops it (mirror boardTickMsg).
type wfLiveTickMsg struct{ epoch int }

// loadWfLight reads only the workflow data (no teammate discovery, no pane capture).
func loadWfLight(epoch int) tea.Cmd {
	return func() tea.Msg {
		all, _ := subagent.ListJobs()
		runs, activity, _ := loadWfData(all)
		return wfRefreshMsg{jobs: wfTagged(all), runs: runs, activity: activity, epoch: epoch}
	}
}

// wfLiveTick schedules the next light refresh.
func wfLiveTick(epoch int) tea.Cmd {
	return tea.Tick(workflowsLiveInterval, func(time.Time) tea.Msg {
		return wfLiveTickMsg{epoch: epoch}
	})
}

// startWfLive starts the 500ms light chain when a refresh sees a running leaf and no chain
// is already live; the chain stops itself (clearing the flag) once nothing runs.
func (m *Model) startWfLive() tea.Cmd {
	if m.wfLiveOn || !m.anyLeafRunning() {
		return nil
	}
	m.wfLiveOn = true
	return wfLiveTick(m.boardEpoch)
}

func leadSessionIDs(teammates []teardown.Teammate, jobs []subagent.Result, runs []subagent.WorkflowRun) []string {
	ids := make([]string, 0, len(teammates)+len(jobs)+len(runs))
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
	for _, r := range runs {
		if r.SessionID != "" {
			ids = append(ids, r.SessionID)
		}
	}
	return ids
}

// groupByTeam returns ts stably sorted by LeadSessionID, then Team, so the
// session tree buckets contiguous runs of one team. Session order is the
// earliest teammate SpawnTime observed for that session; empty sessions sort
// last. Team order is the earliest SpawnTime within that session. Stable
// sorting preserves input order as the final tiebreaker.
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

// asSession is one Claude session's slice of the Agents Board: its teams (each with
// the session's teammates, in groupByTeam order) and its standalone (RunID == "") subagent
// jobs — the single tree every level indexes. earliest (the first teammate SpawnTime / job
// StartedAt) is the "created" display; latest is the newest-first sort key.
type asSession struct {
	sessionID string
	runs      []runGroup
	teams     []asTeam
	jobs      []subagent.Result
	earliest  time.Time // first activity — the "created" display
	latest    time.Time // most recent activity — the newest-first sort key
	hasTime   bool
}

// asTeam is one team's members within a session.
type asTeam struct {
	name    string
	members []teardown.Teammate
}

// asRailRef identifies an entity collection by TYPE — a team (by name) or the session's
// subagent jobs — so a real team named "jobs" can never be confused with the jobs
// collection.
type asRailRef struct {
	jobs bool
	team string
}

// asProject is one project directory's slice of the board: the sessions whose transcripts
// record that working directory ("" = unresolvable, the "(no project)" bucket).
type asProject struct {
	dir      string
	sessions []asSession
}

// sessionProjectDir is a session's project bucket key: the lead session's recorded cwd,
// else (for a runs-only session) the run manifest's launch cwd.
func sessionProjectDir(s asSession, meta map[string]sessiontitle.Meta) string {
	if dir := meta[s.sessionID].Cwd; dir != "" {
		return dir
	}
	if len(s.runs) > 0 {
		return s.runs[0].cwd
	}
	return ""
}

// groupProjects buckets the (already newest-first) sessions by their recorded working
// directory. First-seen project order therefore follows each project's most recently
// active session; the unknown bucket sorts last.
func groupProjects(sessions []asSession, meta map[string]sessiontitle.Meta) []asProject {
	order := []string{}
	byDir := map[string]*asProject{}
	for _, s := range sessions {
		dir := sessionProjectDir(s, meta)
		p, ok := byDir[dir]
		if !ok {
			p = &asProject{dir: dir}
			byDir[dir] = p
			order = append(order, dir)
		}
		p.sessions = append(p.sessions, s)
	}
	out := make([]asProject, 0, len(order))
	for _, dir := range order {
		out = append(out, *byDir[dir])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].dir == "") != (out[j].dir == "") {
			return out[j].dir == ""
		}
		return false
	})
	return out
}

// asBoxRef identifies an L2 continuum row by typed identity — a run by id, a team by name,
// a job by id — so the cursor can re-find its row after a refresh shifts the indices.
// isTeam carries the team kind explicitly: "" is a VALID team name (the "(no team)"
// bucket), so it can't double as the not-a-team marker.
type asBoxRef struct {
	isTeam bool
	team   string
	jobID  string
	runID  string
}

// groupSessions builds the board's session tree: teammates (pre-ordered by groupByTeam)
// bucket into session → team in encounter order; RunID-tagged jobs feed the runs (each
// session's workflow runs, via groupByRun) instead of the Subagents box. Sessions sort
// NEWEST-FIRST by latest activity — defined for any mix of teams/jobs/runs; a session with
// no parseable timestamp sorts after timed ones, and "" (no session) always last.
func groupSessions(teammates []teardown.Teammate, jobs []subagent.Result, runs []runGroup) []asSession {
	order := []string{}
	byID := map[string]*asSession{}
	ensure := func(id string) *asSession {
		if s, ok := byID[id]; ok {
			return s
		}
		s := &asSession{sessionID: id}
		byID[id] = s
		order = append(order, id)
		return s
	}
	noteTime := func(s *asSession, t time.Time) {
		if !s.hasTime || t.Before(s.earliest) {
			s.earliest = t
		}
		if !s.hasTime || t.After(s.latest) {
			s.latest = t
		}
		s.hasTime = true
	}
	for _, t := range groupByTeam(teammates) {
		s := ensure(t.LeadSessionID)
		if n := len(s.teams); n == 0 || s.teams[n-1].name != t.Team {
			s.teams = append(s.teams, asTeam{name: t.Team})
		}
		tm := &s.teams[len(s.teams)-1]
		tm.members = append(tm.members, t)
		if t.SpawnTime > 0 {
			noteTime(s, time.UnixMilli(t.SpawnTime))
		}
	}
	for _, j := range jobs {
		if j.RunID != "" {
			continue
		}
		s := ensure(j.LeadSessionID)
		s.jobs = append(s.jobs, j)
		if ts, err := time.Parse(time.RFC3339, j.StartedAt); err == nil {
			noteTime(s, ts)
		}
	}
	for _, g := range runs {
		s := ensure(g.sessionID)
		s.runs = append(s.runs, g)
		if ts, err := time.Parse(time.RFC3339, g.startedAt); err == nil {
			noteTime(s, ts)
		}
	}
	out := make([]asSession, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.sessionID == "") != (b.sessionID == "") {
			return b.sessionID == "" // "(no session)" always last
		}
		if a.hasTime != b.hasTime {
			return a.hasTime
		}
		if a.hasTime && !a.latest.Equal(b.latest) {
			return a.latest.After(b.latest)
		}
		return false // stable: encounter order as the tiebreaker
	})
	return out
}

// boardTick schedules the next auto-refresh tick for the given epoch.
func boardTick(epoch int) tea.Cmd {
	return tea.Tick(boardRefreshInterval, func(time.Time) tea.Msg {
		return boardTickMsg{epoch: epoch}
	})
}

// stopRunCmd reaps + stops the focused run and reports the outcome via its workflowCtlMsg. It is the
// board's only run-state mutation besides restart. The epoch stamps the originating board visit so a
// result landing after the user left + re-entered (epoch++) is dropped (mirror boardMsg's gate).
func stopRunCmd(runID string, epoch int) tea.Cmd {
	return func() tea.Msg {
		err := subagent.WithRunLock(runID, func() error {
			_, e := subagent.StopRun(runID)
			return e
		})
		return workflowCtlMsg{verb: "stop", runID: runID, err: err, epoch: epoch}
	}
}

// leafCtlCmd sends a LIVE leaf directive (stop-leaf / restart-leaf) over the run's
// control plane and reports via workflowCtlMsg — the engine's poller applies it and the
// effect surfaces through the board poll (held ‖ / a fresh attempt).
func leafCtlCmd(verb, runID, leafID string, epoch int) tea.Cmd {
	op := "stop"
	if verb == "restart-leaf" {
		op = "restart"
	}
	return func() tea.Msg {
		err := workflow.SendLeafCommand(runID, op, leafID)
		return workflowCtlMsg{verb: verb, runID: runID, err: err, epoch: epoch}
	}
}

// phaseCtlCmd sends a LIVE phase directive over the run's control plane.
func phaseCtlCmd(verb, runID, phase string, epoch int) tea.Cmd {
	op := "stop"
	if verb == "restart-phase" {
		op = "restart"
	}
	return func() tea.Msg {
		err := workflow.SendPhaseCommand(runID, op, phase)
		return workflowCtlMsg{verb: verb, runID: runID, err: err, epoch: epoch}
	}
}

// restartPhaseCmd is the keyed phase restart for a TERMINAL run (drops the phase's
// journal-key set and resumes).
func restartPhaseCmd(runID, phase string, epoch int) tea.Cmd {
	return func() tea.Msg {
		_, err := workflow.RestartPhase(context.Background(), runID, phase)
		return workflowCtlMsg{verb: "restart-phase", runID: runID, err: err, epoch: epoch}
	}
}

// restartCmd restarts a run via workflow.Restart: an empty journalKey resumes the WHOLE run
// (re-running only un-journaled / failed leaves); a leaf's journalKey additionally drops that leaf's
// cache so the resume re-runs it (+ any downstream leaf whose input shifted). On a still-running run
// the engine is stopped first, so every in-flight sibling that had not journaled re-runs too — a keyed
// restart is scoped to the one leaf only once the run is already terminal. workflow.Restart replays the
// run's original launch options off the manifest. epoch gates a stale result like stopRunCmd.
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
		err := subagent.WithRunLock(runID, func() error { return subagent.PurgeRun(runID) })
		return workflowCtlMsg{verb: "delete", runID: runID, err: err, epoch: epoch}
	}
}

// workflowCtlMsg carries the outcome of a run control (stop/restart from the drill, delete/save from
// the run row). Its handler resolves the running modal (stop/restart) or pops an info modal
// (delete/save), then reloads. Owned by screenSpawn; epoch gates a stale prior-visit result.
type workflowCtlMsg struct {
	verb  string // "stop" | "restart" | "delete" | "save"
	runID string
	err   error
	epoch int
}

func (workflowCtlMsg) owningScreen() screen { return screenSpawn }

// ctlOutcome formats a run-control result line and whether it failed — shared by the modal-resolve
// path (a stop/restart confirmed in the modal) and the info-popup path (a delete/save outcome).
func ctlOutcome(verb, runID string, err error) (string, bool) {
	if err != nil {
		return fmt.Sprintf("%s %s failed: %s", verb, runID, sessiontitle.CleanTitle(err.Error())), true
	}
	switch verb {
	case "stop-leaf":
		return "agent stop sent — it holds (‖) until you restart it", false
	case "restart-leaf":
		return "agent restart sent — it re-runs in place", false
	case "stop-phase":
		return "phase stop sent — its agents hold (‖) until you restart the phase", false
	case "restart-phase":
		return "phase restart sent", false
	}
	return fmt.Sprintf("%s %s: ok", verb, runID), false
}

// runningVerb is the workflowCtlMsg verb a running modal of the given kind awaits, so a shared-channel
// outcome for a different op on the same run (e.g. a save) can't resolve it.
func runningVerb(kind string) string {
	switch kind {
	case confirmStop:
		return "stop"
	case confirmRestart, confirmRestartAgent:
		return "restart"
	case confirmStopLeaf:
		return "stop-leaf"
	case confirmRestartLeaf:
		return "restart-leaf"
	case confirmStopPhase:
		return "stop-phase"
	case confirmRestartPhase, confirmRestartPhaseKeyed:
		return "restart-phase"
	}
	return ""
}

// wfDetailMsg carries the focused leaf's io read for the inline agent-detail pane: the prompt +
// answer (already read off the Update goroutine) and whether either io file was present. Owned by
// screenSpawn (the agent detail is inline); nonce is the focused-leaf request it answers, so a
// slow read for a previously-focused leaf is dropped rather than shown on the wrong agent.
type wfDetailMsg struct {
	nonce   int
	epoch   int
	job     subagent.Result
	prompt  string
	answer  string
	present bool
}

func (wfDetailMsg) owningScreen() screen { return screenSpawn }

// loadLeafIOCmd reads the selected leaf's prompt/answer side files
// (<ConfigDir>/subagent-jobs/<jobID>.prompt / .answer; 0600, present only when
// persist-io was on). A read failure (absent files) degrades to empty + present
// false so the inline detail shows the not-persisted note. The answer text reaches ONLY the
// focused agent's inline detail pane — never the board's agent rows. nonce tags the request so a
// stale read can't populate a later leaf's detail.
func loadLeafIOCmd(job subagent.Result, nonce, epoch int) tea.Cmd {
	return func() tea.Msg {
		prompt, answer, present := readLeafIO(job.JobID)
		return wfDetailMsg{nonce: nonce, epoch: epoch, job: job, prompt: prompt, answer: answer, present: present}
	}
}

// anyLeafRunning reports whether any workflow activity is live (drives the live tick
// cadence). A run whose every leaf is held has no running LEAF but is still live —
// its manifest says running — so the run status is consulted too.
func (m Model) anyLeafRunning() bool {
	for _, j := range m.workflowJobs {
		if j.Status == "running" {
			return true
		}
	}
	for _, r := range m.workflowRuns {
		if r.Status == "running" {
			return true
		}
	}
	return false
}

// paneVisMsg carries the outcome of an inline hide/show so the board can surface
// a failure (its code/reason/suggestion) instead of silently relying on the next
// refresh to show an unchanged HIDDEN column. Its handler pops an info modal and
// then reloads the board to reflect the new state. Owned by screenSpawn so a
// result landing after the user left the board can't pop a stale modal on the hub.
type paneVisMsg struct{ res panevis.Result }

func (paneVisMsg) owningScreen() screen { return screenSpawn }

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

// asDetailMsg carries the focused standalone job's io + activity read for the entity detail
// card (mirror wfDetailMsg). Owned by screenSpawn; nonce gates out a stale read so a slow
// job-A read landing after the user moved to job-B is dropped.
type asDetailMsg struct {
	nonce   int
	epoch   int
	jobID   string
	prompt  string
	answer  string
	present bool
	snap    activitySnapshot
}

func (asDetailMsg) owningScreen() screen { return screenSpawn }

// loadJobIOCmd reads the job's prompt/answer side files + activity sidecar off the Update
// goroutine (mirror loadLeafIOCmd). The answer text reaches ONLY the focused job's inline
// detail card — never a board row.
func loadJobIOCmd(jobID string, nonce, epoch int) tea.Cmd {
	return func() tea.Msg {
		prompt, answer, present := readLeafIO(jobID)
		snap, _ := readLeafActivity(jobID)
		return asDetailMsg{nonce: nonce, epoch: epoch, jobID: jobID, prompt: prompt, answer: answer, present: present, snap: snap}
	}
}

// asMateMsg carries the focused teammate's merged inbox + transcript projection for the
// entity detail card (the asDetailMsg pattern: owned by screenSpawn, gated by the shared
// asDetailNonce + the board epoch).
type asMateMsg struct {
	nonce int
	epoch int
	key   string
	snap  teammateSnapshot
	found bool
}

func (asMateMsg) owningScreen() screen { return screenSpawn }

// mateKey identifies a teammate's detail payload by (team, name, pid). The pid is the
// generation component: a respawned same-named teammate gets a new process, so a payload
// loaded for its predecessor can never render on the new card while the fresh load is in
// flight. \x00 cannot appear in the name components.
func mateKey(t teardown.Teammate) string {
	return fmt.Sprintf("%s\x00%s\x00%d", t.Team, t.Name, t.PID)
}

// loadMateCmd reads the teammate's transcript projection + pending inbox entries off the
// Update goroutine. The transcript is rediscovered on every load — a cached path could
// outlive a respawned same-named teammate — with the scan bounded to files modified since
// the teammate's spawn (its own transcript keeps being appended, so it can never age past
// that). Unread inbox entries merge into the message list as the PENDING backlog (read
// ones already live in the transcript); the merged list sorts chronologically. Message and
// transcript text reach ONLY the focused teammate's inline detail card.
func loadMateCmd(t teardown.Teammate, cwd string, nonce, epoch int) tea.Cmd {
	return func() tea.Msg {
		var notBefore time.Time
		if t.SpawnTime > 0 {
			notBefore = time.UnixMilli(t.SpawnTime)
		}
		var snap teammateSnapshot
		found := false
		if path, ok := sessiontitle.FindAgentTranscript(cwd, t.Team, t.Name, notBefore); ok {
			snap, found = readTeammateTranscript(path)
		}
		for _, e := range readTeammateInbox(t.Team, t.Name) {
			if e.Read {
				continue
			}
			snap.msgs = append(snap.msgs, mateMessage{
				from: e.From, summary: inboxPreview(e.Text), body: e.Text, ts: e.Timestamp, pending: true,
			})
		}
		// Parse for the chronological sort: the sources mix timestamp precision
		// (inbox millis vs transcript seconds), so a raw string compare can misorder
		// within a second; unparseable stamps keep their arrival order (stable).
		sort.SliceStable(snap.msgs, func(i, j int) bool {
			ti, ei := time.Parse(time.RFC3339, snap.msgs[i].ts)
			tj, ej := time.Parse(time.RFC3339, snap.msgs[j].ts)
			if ei != nil || ej != nil {
				return false
			}
			return ti.Before(tj)
		})
		return asMateMsg{nonce: nonce, epoch: epoch, key: mateKey(t), snap: snap, found: found}
	}
}

// asTeamMsg carries a cursored team's summed member transcript tokens for the session
// header (the asMateMsg pattern: owned by screenSpawn, nonce+epoch gated).
type asTeamMsg struct {
	nonce int
	epoch int
	key   string
	ctx   int
	out   int
}

func (asTeamMsg) owningScreen() screen { return screenSpawn }

// loadTeamStatsCmd scans every member's transcript off the Update goroutine and sums the
// token aggregates (each member's ↑ peak context; cumulative ↓ output). Only integer counts
// leave the read.
func loadTeamStatsCmd(t asTeam, cwdOf map[string]sessiontitle.Meta, nonce, epoch int) tea.Cmd {
	members := append([]teardown.Teammate(nil), t.members...)
	return func() tea.Msg {
		ctx, out := 0, 0
		for _, mem := range members {
			var notBefore time.Time
			if mem.SpawnTime > 0 {
				notBefore = time.UnixMilli(mem.SpawnTime)
			}
			path, ok := sessiontitle.FindAgentTranscript(cwdOf[mem.LeadSessionID].Cwd, mem.Team, mem.Name, notBefore)
			if !ok {
				continue
			}
			if snap, ok := readTeammateTranscript(path); ok {
				ctx += snap.ctxTok
				out += snap.outTok
			}
		}
		return asTeamMsg{nonce: nonce, epoch: epoch, key: t.name, ctx: ctx, out: out}
	}
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

// codexRollbackDoneMsg reports a finished abandon-rollback (a codex row removed after
// its login was cancelled). Distinct from opDoneMsg so the cancel lands quietly on the
// provider list rather than a "remove OK" outcome modal; epoch-tagged so a stale one
// can't disturb a newer attempt.
type codexRollbackDoneMsg struct{ epoch int }

func (codexRollbackDoneMsg) owningScreen() screen { return screenCodexAuth }

// codexRollbackCmd removes a codex row committed ahead of a login the user then
// cancelled (best-effort: the row removal is what matters; its empty login is cleaned
// by Remove's own LogoutIfUnreferenced).
func codexRollbackCmd(name string, epoch int) tea.Cmd {
	return func() tea.Msg {
		_, _ = userops.Remove(userops.RemoveRequest{Name: name})
		return codexRollbackDoneMsg{epoch: epoch}
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
		// Group the list by wire class (stable, so the name order within a class
		// that List returns is preserved).
		sort.SliceStable(m.vendors, func(i, j int) bool {
			return vendorClassRank(m.vendors[i].Protocol) < vendorClassRank(m.vendors[j].Protocol)
		})
		m.vendorsErr = msg.err
		// The cursor may also rest on the trailing "+ Add provider…" row at index
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
		// A board entry routes from THIS fresh load: zero the cursors and park the
		// focus now (deferred from the tab handler so the stale frame could render).
		if m.boardEntryRoute {
			m.boardEntryRoute = false
			m.asProjectCursor, m.asSessionCursor, m.asBoxCursor, m.asEntityCursor, m.asCardScroll = 0, 0, 0, 0, 0
			m.focusedProject, m.focusedSessionID, m.asMode = asNoFocus, asNoFocus, asModeBoxes
		}
		// Capture the L2 cursor row's TYPED identity before the data changes, so the
		// cursor can re-find it (a team by name, a job by id) after the refresh.
		prevBox, hadBox := m.boxRowRef()
		m.loading = false
		m.boardSeen = true
		// Pre-group session → team so the session tree (recomputed per render off these
		// slices, mirror wfGroups) renders contiguously on every update path, tests included.
		m.teammates = groupByTeam(msg.teammates)
		m.spawnErr = msg.teamErr
		m.jobs = msg.jobs
		m.boardJobsErr = msg.jobsErr
		m.workflowJobs = wfTagged(msg.jobs)
		m.workflowRuns = msg.runs
		m.wfActivity = msg.activity
		m.sessionMeta = msg.sessionMeta
		m.endedSeen = msg.endedSeen
		m.pins = msg.pins
		// Preserve the drill state: re-route if the focus chain broke, re-find the L2 row
		// by identity, index-clamp the entity cursor, re-clamp the card scroll.
		m.rerootSpawn()
		if hadBox {
			m.reanchorBox(prevBox)
		}
		m.clampAsCursors()
		m.asCardScroll = m.clampAsCardScroll(m.asCardScroll)
		// Load (or live-refresh) the focused entity's detail payload — a reroot can land
		// the board straight on a detail view. Jobs: a non-terminal focused job re-reads
		// each refresh so its Activity feed climbs, and the load that FIRST sees the job
		// terminal still runs once, so the final .answer lands after the running→done
		// flip. Teammates: long-lived, their inbox/transcript keep moving — reload on
		// every accepted refresh, no terminal short-circuit.
		var detail tea.Cmd
		if m.asMode == asModeEntity {
			if m.asEntitySrc.jobs {
				if j, ok := m.selectedJob(); ok && m.jobCardStale(j) {
					m.asDetailNonce++
					m.asDetailTerminal = isTerminalLeaf(j.Status)
					detail = loadJobIOCmd(j.JobID, m.asDetailNonce, m.boardEpoch)
				}
			} else if t, ok := m.selectedTeammate(); ok {
				m.asDetailNonce++
				detail = loadMateCmd(t, m.sessionMeta[t.LeadSessionID].Cwd, m.asDetailNonce, m.boardEpoch)
			}
		} else if m.asMode == asModeBoxes {
			// The boxes level always previews a job card (the cursored job, else the
			// first) — keep it fresh on the same terms as the entity-level card, and a
			// cursored team row keeps its header aggregate fresh in the same batch.
			cmds := []tea.Cmd{m.focusBoxTeamIO()}
			if j, ok := m.boxPreviewJob(); ok && m.jobCardStale(j) {
				m.asDetailNonce++
				m.asDetailTerminal = isTerminalLeaf(j.Status)
				cmds = append(cmds, loadJobIOCmd(j.JobID, m.asDetailNonce, m.boardEpoch))
			}
			detail = tea.Batch(cmds...)
		}
		return m, tea.Batch(detail, m.startWfLive())

	case paneVisMsg:
		// Pop the hide/show outcome as an info modal, then reload so the HIDDEN column reflects the
		// new state. m.confirm survives the immediate refresh (boardMsg never touches it).
		r := msg.res
		if r.OK {
			m = m.withInfo(fmt.Sprintf("%s %s: ok", r.Action, r.Name), false)
		} else {
			line := fmt.Sprintf("%s %s failed: %s %s", r.Action, r.Name, r.ErrorCode, r.ErrorMsg)
			if r.Suggestion != "" {
				line += " — " + r.Suggestion
			}
			m = m.withInfo(line, true)
		}
		return m, loadBoard(m.boardEpoch)

	case boardTickMsg:
		// Only the current tick chain, and only while the board is open, keeps
		// refreshing — a stale or off-board tick stops the chain.
		if m.screen == screenSpawn && msg.epoch == m.boardEpoch {
			return m, tea.Batch(loadBoard(msg.epoch), boardTick(msg.epoch))
		}
		return m, nil

	case wfRefreshMsg:
		// The light chain's payload: only the workflow halves move; the cursor re-anchors
		// exactly like a full refresh (a finishing run reshapes the continuum).
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		prevBox, hadBox := m.boxRowRef()
		m.workflowJobs = msg.jobs
		m.workflowRuns = msg.runs
		m.wfActivity = msg.activity
		m.rerootSpawn()
		if hadBox {
			m.reanchorBox(prevBox)
		}
		m.clampAsCursors()
		m.asCardScroll = m.clampAsCardScroll(m.asCardScroll)
		return m, m.startWfLive()

	case wfLiveTickMsg:
		if m.screen == screenSpawn && msg.epoch == m.boardEpoch {
			if m.anyLeafRunning() {
				return m, tea.Batch(loadWfLight(msg.epoch), wfLiveTick(msg.epoch))
			}
			m.wfLiveOn = false // nothing runs — the chain ends; a later refresh restarts it
		}
		return m, nil

	case workflowCtlMsg:
		// A stale result from a prior board visit must not mutate a fresh one.
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		if msg.verb != "save" {
			delete(m.wfRestarting, msg.runID) // stop/restart/delete completed — clear the in-flight guard (save never set it)
		}
		// The run id + error are opaque/operator-supplied text, so scrub them before display.
		runID := shortRunID(sessiontitle.CleanTitle(msg.runID))
		line, isErr := ctlOutcome(msg.verb, runID, msg.err)
		// A stop/restart confirmed via the modal waits in modalRunning for exactly this id AND verb —
		// resolve it in place. Otherwise (a delete/save dispatched on close, or a same-id result for a
		// different verb) pop a fresh info modal; withInfo no-ops if a modal is up so it can't clobber.
		if m.confirm != nil && m.confirm.phase == modalRunning && m.confirm.id == msg.runID && msg.verb == runningVerb(m.confirm.kind) {
			c := *m.confirm
			c.phase, c.result, c.resultErr = modalResult, line, isErr
			m.confirm = &c
		} else {
			m = m.withInfo(line, isErr)
		}
		return m, loadWfLight(m.boardEpoch)

	case teamHistCtlMsg:
		// A stale result from a prior board visit must not mutate a fresh one.
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		// Pop the delete outcome as an info modal, then reload so the now-gone team drops off. The
		// team name is config-sourced, so scrub it like every other board surface before display.
		team := sessiontitle.CleanTitle(displayTeam(msg.team))
		if msg.err != nil {
			m = m.withInfo(fmt.Sprintf("delete %s failed: %s", team, sessiontitle.CleanTitle(msg.err.Error())), true)
		} else {
			m = m.withInfo(fmt.Sprintf("delete %s: ok", team), false)
		}
		return m, loadBoard(m.boardEpoch)

	case jobCtlMsg:
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		if msg.err != nil {
			m = m.withInfo("delete job failed: "+sessiontitle.CleanTitle(msg.err.Error()), true)
		} else {
			m = m.withInfo("delete job: ok", false)
		}
		return m, loadBoard(m.boardEpoch)

	case sessionDelMsg:
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		line, isErr := fmt.Sprintf("deleted %d record(s) · %d ended team(s)", msg.removed, msg.teams), false
		if msg.err != nil {
			line, isErr = "delete session failed: "+sessiontitle.CleanTitle(msg.err.Error()), true
		}
		// Resolve the running danger modal in place; otherwise (none open) pop an info modal.
		if m.confirm != nil && m.confirm.phase == modalRunning && m.confirm.kind == confirmSession {
			c := *m.confirm
			c.phase, c.result, c.resultErr = modalResult, line, isErr
			m.confirm = &c
		} else {
			m = m.withInfo(line, isErr)
		}
		return m, loadBoard(m.boardEpoch)

	case asDetailMsg:
		// Drop a read that answers a prior focused job or a prior board visit (the
		// wfDetailMsg nonce gate + the board's epoch gate).
		if msg.nonce != m.asDetailNonce || msg.epoch != m.boardEpoch {
			return m, nil
		}
		m.asDetailJobID = msg.jobID
		m.asDetailPrompt = msg.prompt
		m.asDetailAnswer = msg.answer
		m.asDetailIO = msg.present
		m.asDetailSnap = msg.snap
		return m, nil

	case asTeamMsg:
		if msg.nonce != m.asTeamNonce || msg.epoch != m.boardEpoch {
			return m, nil
		}
		m.asTeamKey, m.asTeamCtx, m.asTeamOut = msg.key, msg.ctx, msg.out
		return m, nil

	case asMateMsg:
		// Same gate as asDetailMsg: a read answering a prior focused entity or a prior
		// board visit is dropped, never shown on the wrong teammate's card.
		if msg.nonce != m.asDetailNonce || msg.epoch != m.boardEpoch {
			return m, nil
		}
		m.asMateKey = msg.key
		m.asMateSnap = msg.snap
		m.asMateFound = msg.found
		return m, nil

	case wfDetailMsg:
		// Drop a read that answers a prior focused leaf or a prior board visit (nonce +
		// epoch gates) so the inline detail never shows the wrong leaf's answer.
		if msg.nonce != m.wfDetailNonce || msg.epoch != m.boardEpoch {
			return m, nil
		}
		m.wfDetailJob = msg.job
		m.wfDetailPrompt = msg.prompt
		m.wfDetailAnswer = msg.answer
		m.wfDetailIO = msg.present
		return m, nil

	case modelsMsg:
		m.loading = false
		m.modelList = msg.models
		m.modelsErr = msg.err
		m.modelCursor = 0
		return m, nil

	case opDoneMsg:
		// While a marked codex login flow owns the screen (committing, consent, or
		// device), a stale result from an earlier, unrelated op is dropped outright:
		// yanking the user to an outcome modal would free them to start another codex
		// commit and overwrite the single marker, silently orphaning the first row
		// without login or rollback. Only the committed add's own result moves the
		// flow; only its failure clears the marker.
		if m.screen == screenCodexAuth && m.codexCommittedName != "" &&
			!(msg.verb == "add" && msg.name == m.codexCommittedName) {
			return m, nil
		}
		m.loading = false
		if msg.verb == "add" && m.codexCommittedName != "" && msg.name == m.codexCommittedName {
			if msg.err == nil {
				m.screen = screenCodexAuth
				m.codexAuthStage = codexAuthConsent
				m.codexAuthErr = ""
				return m, nil
			}
			m.codexCommittedName = ""
		}
		// The outcome pops as an info modal over the list (green ok / red failure, any
		// key dismisses) while the list reloads underneath — loading stays false so the
		// previous frame, not a bare "loading…", is the underlay.
		m.screen = screenList
		line := fmt.Sprintf("%s %q: OK", msg.verb, msg.name)
		isErr := false
		if msg.err != nil {
			line = fmt.Sprintf("%s %q failed: %v", msg.verb, msg.name, msg.err)
			isErr = true
		}
		return m.withInfo(line, isErr), loadVendors

	case codexAuthBegunMsg:
		// Drop a begin from a prior login attempt (esc then re-enter starts a new
		// epoch); the owningScreen gate alone can't tell visits apart.
		if msg.epoch != m.codexAuthEpoch {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.codexAuthErr = msg.err.Error()
			return m, nil
		}
		m.codexAuth = msg.session
		return m, pollCodexAuthCmd(m.codexAuthCtx, m.codexAuthEpoch, msg.session)

	case codexAuthPollMsg:
		// A stale-epoch result (the user abandoned and the epoch advanced) or one with no
		// live session is dropped before it can show success for a torn-down attempt.
		if msg.epoch != m.codexAuthEpoch || m.codexAuth == nil {
			return m, nil
		}
		if msg.err != nil {
			m.codexAuthErr = msg.err.Error()
			m.codexAuth = nil
			return m, nil
		}
		if msg.done {
			// The provider row was already committed before the login (see
			// submitAddCodex); the login just filled its credential, so report success
			// as an info modal over the reloading list.
			if m.codexAuthCancel != nil {
				m.codexAuthCancel()
				m.codexAuthCancel, m.codexAuthCtx = nil, nil
			}
			m.codexAuth = nil
			name := m.codexCommittedName
			m.codexCommittedName = ""
			m.loading = false
			m.screen = screenList
			return m.withInfo(fmt.Sprintf("add %q: OK", name), false), loadVendors
		}
		return m, codexAuthTickCmd(m.codexAuthEpoch, m.codexAuth.Interval())

	case codexAuthTickMsg:
		if msg.epoch != m.codexAuthEpoch || m.codexAuth == nil || m.screen != screenCodexAuth {
			return m, nil
		}
		return m, pollCodexAuthCmd(m.codexAuthCtx, m.codexAuthEpoch, m.codexAuth)

	case codexRollbackDoneMsg:
		// Drop a rollback completion from an abandoned flow whose epoch has since moved
		// on, so it can't yank a newer attempt back to the list.
		if msg.epoch != m.codexAuthEpoch {
			return m, nil
		}
		m.loading = false
		return m.toList()

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
		case screenPickTemplate:
			return m.updatePickTemplate(msg)
		case screenForm:
			return m.updateForm(msg)
		case screenModelPick:
			return m.updateModelPick(msg)
		case screenKeys:
			return m.updateKeys(msg)
		case screenSetup:
			return m.updateSetup(msg)
		case screenSetupTmux:
			return m.updateSetupTmux(msg)
		case screenCodexAuth:
			return m.updateCodexAuth(msg)
		}
	}
	return m, nil
}

// toList returns to the Model Providers list (the hub) and reloads it — after an
// add/edit/remove the content changed, and a plain cancel just re-reads. Any open
// hub modal is dropped with the screen it overlaid.
func (m Model) toList() (tea.Model, tea.Cmd) {
	m.screen = screenList
	m.loading = true
	m.confirm = nil
	return m, loadVendors
}

// updateList drives the Model Providers hub. The cursor ranges over [0, len(vendors)];
// the final index is the synthetic "+ Add provider…" row. →/enter edits the
// highlighted provider (or opens the add wizard on the Add row); d deletes it
// (via a confirm modal); tab switches to the Agents Board; q/esc quit.
func (m Model) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg) // the remove-confirm modal traps focus until resolved
	}
	addRow := len(m.vendors) // index of the trailing "+ Add provider…" row
	switch msg.String() {
	case "q", "esc":
		m.quitting = true
		return m, tea.Quit
	case "tab":
		m.screen = screenSpawn
		m.loading = !m.boardSeen // a revisit keeps the previous frame until the fresh load lands
		m.boardJobsErr = nil
		m.asDetailJobID, m.asDetailPrompt, m.asDetailAnswer, m.asDetailIO = "", "", "", false
		m.asDetailSnap, m.asPromptExpanded = activitySnapshot{}, false
		m.asMateKey, m.asMateSnap, m.asMateFound = "", teammateSnapshot{}, false
		m.wfLiveOn = false
		// Per-visit run/record control state: a ctl result dropped while off-screen
		// must not leave a stale busy flag or an open modal on the next visit.
		m.confirm = nil
		m.wfRestarting = map[string]bool{}
		m.wfSaving = false
		// The entry reset (cursor zeroing + the unfocused park that makes the FIRST
		// accepted refresh route by the fresh counts) is deferred to that refresh via
		// boardEntryRoute, so the previous frame renders meanwhile instead of a flash.
		m.boardEntryRoute = true
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
	case "enter", "right":
		if m.vendorCursor == addRow {
			m.screen = screenPickTemplate
			m.tmplCursor = 0
			return m, nil
		}
		v := m.vendors[m.vendorCursor]
		m.form = newEditForm(v)
		m.formMode = modeEdit
		m.editName = v.Name
		m.addProtocol = v.Protocol // the form's provider class (drives the codex model picker)
		m.screen = screenForm
		return m, textinput.Blink
	case "d":
		// No new ask while a mutation is in flight: its outcome modal would find this
		// one open and be silently swallowed (withInfo never clobbers an open modal).
		if m.vendorCursor < addRow && !m.loading { // a vendor row, not the Add row
			v := m.vendors[m.vendorCursor]
			// State the real consequences per class: a codex remove tears down cc-fleet's
			// own login (when unreferenced), never ~/.codex; a file-backend remove drops
			// the secret + key store.
			prompt := "Remove " + v.Name + "? Deletes its config row, profile, and (file backend) its secret + key store."
			if v.Protocol == config.ProtocolCodexOAuth {
				prompt = "Remove " + v.Name + "? Deletes its profile and cc-fleet's codex login (if unused elsewhere); ~/.codex is untouched."
			}
			return m.openConfirm(confirmRemoveVendor, v.Name, prompt)
		}
	}
	return m, nil
}

// updateSpawn drives the master-detail Agents Board, branching on asMode (projects,
// sessions, the session's boxes, an entity's inline detail, or a run drill). r reloads
// everywhere except the run drill, where it is restart and R/ctrl+r reload; tab returns to
// the Model Providers hub; q quits. The per-mode handlers own ↑/↓, →/⏎ (descend), ←/esc
// (ascend — esc additionally leaves for the hub at the board's top level), and h/s (entity
// mode, teammate rows). The auto-refresh tick chain runs independently (see boardTickMsg).
func (m Model) updateSpawn(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.wfSaving {
		return m.updateWfSaveInput(msg)
	}
	if m.confirm != nil {
		return m.updateConfirm(msg) // a confirm modal traps focus: ←/→ select, enter runs the choice (no esc)
	}
	atRunLevel := m.asMode == asModeRunPhases || m.asMode == asModeRunAgent
	switch msg.String() {
	case "r":
		if atRunLevel {
			break // lowercase r is the workflow restart there; R/ctrl+r still refresh
		}
		m.loading = true
		return m, loadBoard(m.boardEpoch)
	case "R", "ctrl+r":
		m.loading = true
		return m, loadBoard(m.boardEpoch)
	case "tab":
		return m.toList()
	case "q":
		m.quitting = true
		return m, tea.Quit
	}
	switch m.asMode {
	case asModeProjects:
		return m.updateAsProjects(msg)
	case asModeSessions:
		return m.updateAsSessions(msg)
	case asModeEntity:
		return m.updateAsEntity(msg)
	case asModeRunPhases:
		return m.updateWfPhases(msg)
	case asModeRunAgent:
		return m.updateWfAgent(msg)
	default:
		return m.updateAsBoxes(msg)
	}
}

// updateAsProjects (L0, >1 project): ↑/↓ choose a project (the right pane previews its
// sessions), →/⏎ descend; esc → Vendors (← clamps at this top level).
func (m Model) updateAsProjects(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	projects := m.asProjects()
	switch msg.String() {
	case "up":
		m.asProjectCursor = clampIndex(m.asProjectCursor-1, len(projects))
	case "down":
		m.asProjectCursor = clampIndex(m.asProjectCursor+1, len(projects))
	case "right", "enter":
		return m.asDescend()
	case "esc":
		return m.asAscend(true)
	case "left":
		return m.asAscend(false)
	}
	return m, nil
}

// updateAsSessions (L1, >1 session in the focused project): ↑/↓ choose a session (the right
// pane shows its overview), →/⏎ descend into its boxes; ←/esc ascend.
func (m Model) updateAsSessions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p, ok := m.focusedProjectGroup()
	switch msg.String() {
	case "up":
		if ok {
			m.asSessionCursor = clampIndex(m.asSessionCursor-1, len(p.sessions))
		}
	case "down":
		if ok {
			m.asSessionCursor = clampIndex(m.asSessionCursor+1, len(p.sessions))
		}
	case "right", "enter":
		return m.asDescend()
	case "d":
		// Delete EVERY record in the cursored session (a live run is stopped first) — a heavy,
		// pin-aware wipe behind a red danger confirm. The "(no session)" bucket has no id to target.
		if ok && m.asSessionCursor >= 0 && m.asSessionCursor < len(p.sessions) {
			if id := p.sessions[m.asSessionCursor].sessionID; id != "" {
				return m.openConfirmDanger(confirmSession, id, "Delete EVERY record in this session? Running ones are stopped.")
			}
		}
	case "esc":
		return m.asAscend(true)
	case "left":
		return m.asAscend(false)
	}
	return m, nil
}

// updateAsBoxes (L2): a single ↑/↓ cursor walks the continuum — the session's run rows
// (the Dynamic Workflows box rail), then its team rows (the Agent Teams box rail), then its
// job rows (the Subagents box). A job row shows its card INLINE in the Subagents box, so it
// gets the card keys right here (j/k scroll, ⏎ fold) and never needs a descend; →/⏎ descend a
// RUN row into its phases or a TEAM row into its member view; d opens a delete confirm for a
// run / ended-team / job row. ←/esc ascend.
func (m Model) updateAsBoxes(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s, ok := m.focusedSession()
	n := 0
	if ok {
		n = len(s.runs) + len(s.teams) + len(s.jobs)
	}
	switch msg.String() {
	case "up":
		m.asBoxCursor = clampIndex(m.asBoxCursor-1, n)
		team := m.focusBoxTeamIO()
		mm, job := m.focusBoxJobIO()
		return mm, tea.Batch(team, job)
	case "down":
		m.asBoxCursor = clampIndex(m.asBoxCursor+1, n)
		team := m.focusBoxTeamIO()
		mm, job := m.focusBoxJobIO()
		return mm, tea.Batch(team, job)
	case "d":
		// A run row confirms a run delete; an ENDED team row its record delete; a job row its
		// delete; a LIVE team row ignores d (it has no removable record). Each opens a modal.
		if g, onRun := m.boxRun(); onRun {
			return m.openConfirm(confirmRun, g.runID, "Delete this run and its agents?")
		}
		if name, onTeam := m.boxTeam(); onTeam && m.isEndedTeam(name) {
			return m.openConfirm(confirmTeam, name, "Delete this ended team's history?")
		}
		if j, onJob := m.boxJob(); onJob {
			return m.openConfirm(confirmJob, j.JobID, "Delete this job?")
		}
	case "p":
		// Toggle the keep/pin on the cursored row (run / team / job — any state). A pinned
		// record survives GC and the clear-finished bulk action until it is unpinned or deleted.
		if kind, id, ok := m.boxPinTarget(); ok {
			return m.togglePin(kind, id)
		}
	case "c":
		// Clear this session's finished (done/failed/stopped) jobs+runs and ended teams,
		// excluding pinned. Needs a real (non-"(no session)") session.
		if s, ok := m.focusedSession(); ok && s.sessionID != "" {
			return m.openConfirm(confirmClear, s.sessionID, "Clear this session's finished tasks?")
		}
	case "s":
		// Save the cursored run as a named, reusable workflow — offered only here at its outermost
		// row (a whole-workflow op belongs to the workflow's row, never inside the run drill).
		if g, onRun := m.boxRun(); onRun {
			return m.startSaveWorkflow(g)
		}
	case "right", "enter":
		if _, onJob := m.boxJob(); onJob {
			if msg.String() == "right" {
				return m, nil // the card is already inline — nothing deeper
			}
			m.asPromptExpanded = !m.asPromptExpanded
			m.asCardScroll = m.clampAsCardScroll(m.asCardScroll)
			return m, nil
		}
		return m.asDescend()
	case "j":
		if _, onJob := m.boxJob(); onJob {
			m.asCardScroll = m.clampAsCardScroll(m.asCardScroll + 1)
		}
	case "k":
		if _, onJob := m.boxJob(); onJob {
			m.asCardScroll = m.clampAsCardScroll(m.asCardScroll - 1)
		}
	case "esc":
		return m.asAscend(true)
	case "left":
		return m.asAscend(false)
	}
	return m, nil
}

// focusBoxTeamIO loads the cursored team's token aggregate for the session header
// (nonce-gated; reloaded on every landing — members keep working).
func (m *Model) focusBoxTeamIO() tea.Cmd {
	t, ok := m.boxTeamGroup()
	if !ok {
		return nil
	}
	m.asTeamNonce++
	return loadTeamStatsCmd(t, m.sessionMeta, m.asTeamNonce, m.boardEpoch)
}

// focusBoxJobIO keeps the Subagents box's previewed card loaded as the L2 cursor moves
// (focusJobIO's boxes-level sibling, nonce-gated): scroll/fold reset when the cursor sits
// on a job row, and a stale preview (different job, or one still running) reloads.
func (m Model) focusBoxJobIO() (tea.Model, tea.Cmd) {
	j, ok := m.boxPreviewJob()
	if !ok {
		return m, nil
	}
	if _, onJob := m.boxJob(); onJob {
		m.asCardScroll = 0
		m.asPromptExpanded = false
	}
	if !m.jobCardStale(j) {
		return m, nil
	}
	m.asDetailNonce++
	m.asDetailTerminal = isTerminalLeaf(j.Status)
	return m, loadJobIOCmd(j.JobID, m.asDetailNonce, m.boardEpoch)
}

// updateAsEntity (L3): ↑/↓ walk the collection's rows (resetting the card scroll), j/k
// scroll the inline detail card, h/s hide/show a teammate row (job rows have no actions);
// ←/esc ascend to the boxes — esc RETURNS from the detail card, it never exits the board
// from here.
func (m Model) updateAsEntity(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	members, jobs := m.railEntities()
	n := len(members) + len(jobs) // one of the two is always nil
	switch msg.String() {
	case "up":
		m.asEntityCursor = clampIndex(m.asEntityCursor-1, n)
		return m.focusEntityIO()
	case "down":
		m.asEntityCursor = clampIndex(m.asEntityCursor+1, n)
		return m.focusEntityIO()
	case "enter":
		// Fold/unfold the focused card's collapsible block (the job's Prompt, the
		// teammate's Messages — both collapsed by default); re-clamp the scroll since
		// the card height changes.
		m.asPromptExpanded = !m.asPromptExpanded
		m.asCardScroll = m.clampAsCardScroll(m.asCardScroll)
		return m, nil
	case "j":
		m.asCardScroll = m.clampAsCardScroll(m.asCardScroll + 1)
	case "k":
		m.asCardScroll = m.clampAsCardScroll(m.asCardScroll - 1)
	case "h":
		// Pass the discovered Teammate row (with its live Socket + PaneID) so HideRef can
		// scope tmux ops to the right server and double-check the pane id against config.
		// An ended row has no live pane — h/s no-op on it.
		if t, ok := m.selectedTeammate(); ok && t.Status != endedStatus {
			return m, hideTeammateCmd(t)
		}
	case "s":
		if t, ok := m.selectedTeammate(); ok && t.Status != endedStatus {
			return m, showTeammateCmd(t)
		}
	case "p":
		// Pin the focused detail row: a job by id, or a teammate's team (team-level pin).
		if j, ok := m.selectedJob(); ok {
			return m.togglePin(pinned.Job, j.JobID)
		}
		if t, ok := m.selectedTeammate(); ok && t.Team != "" {
			return m.togglePin(pinned.Team, t.Team)
		}
	case "d":
		if j, ok := m.selectedJob(); ok {
			return m.openConfirm(confirmJob, j.JobID, "Delete this job?")
		}
	case "c":
		// Clear this session's finished records — also reachable here because a single-kind
		// session auto-skips the boxes level and lands straight on this entity view.
		if s, ok := m.focusedSession(); ok && s.sessionID != "" {
			return m.openConfirm(confirmClear, s.sessionID, "Clear this session's finished tasks?")
		}
	case "esc":
		return m.asAscend(true)
	case "left":
		return m.asAscend(false)
	}
	return m, nil
}

// singleKindSrc reports the one entity collection of a session whose boxes level would
// disambiguate nothing — only jobs, or a single team and no jobs — so navigation skips
// straight to the detail view in both directions.
func singleKindSrc(s asSession) (asRailRef, bool) {
	if len(s.runs) > 0 {
		return asRailRef{}, false // the Dynamic Workflows box must stay reachable
	}
	if len(s.teams) == 0 && len(s.jobs) > 0 {
		return asRailRef{jobs: true}, true
	}
	if len(s.jobs) == 0 && len(s.teams) == 1 && !teamEnded(s.teams[0]) {
		// An ended team keeps the boxes level — its d delete-confirm lives there.
		return asRailRef{team: s.teams[0].name}, true
	}
	return asRailRef{}, false
}

// teamEnded reports whether a team consists solely of history-record members.
func teamEnded(t asTeam) bool {
	for _, mem := range t.members {
		if mem.Status != endedStatus {
			return false
		}
	}
	return len(t.members) > 0
}

// enterSession focuses a session and lands on its boxes — or straight on the entity view
// for a single-kind session (see singleKindSrc).
func (m *Model) enterSession(s asSession) {
	m.focusedSessionID = s.sessionID
	m.asBoxCursor, m.asEntityCursor, m.asCardScroll = 0, 0, 0
	if src, ok := singleKindSrc(s); ok {
		m.asEntitySrc = src
		m.asMode = asModeEntity
		return
	}
	m.asMode = asModeBoxes
}

// asDescend drops one level: projects → sessions (or straight into a sole session),
// sessions → the session's boxes (or its detail view when single-kind), boxes → the
// cursored row's entity detail. The entity level is the deepest.
func (m Model) asDescend() (tea.Model, tea.Cmd) {
	switch m.asMode {
	case asModeProjects:
		projects := m.asProjects()
		if m.asProjectCursor >= len(projects) {
			return m, nil
		}
		p := projects[m.asProjectCursor]
		m.focusedProject = p.dir
		m.asSessionCursor = 0
		if len(p.sessions) == 1 {
			m.enterSession(p.sessions[0])
		} else {
			m.asMode = asModeSessions
		}
	case asModeSessions:
		p, ok := m.focusedProjectGroup()
		if !ok || m.asSessionCursor >= len(p.sessions) {
			return m, nil
		}
		m.enterSession(p.sessions[m.asSessionCursor])
	case asModeBoxes:
		s, ok := m.focusedSession()
		if !ok {
			return m, nil
		}
		switch {
		case m.asBoxCursor < len(s.runs):
			// A run row opens the run's Phases level.
			m.focusedRunID = s.runs[m.asBoxCursor].runID
			m.wfPhaseCursor, m.wfAgentCursor = 0, 0
			m.asMode = asModeRunPhases
			return m, nil
		case m.asBoxCursor-len(s.runs) < len(s.teams):
			m.asEntitySrc = asRailRef{team: s.teams[m.asBoxCursor-len(s.runs)].name}
			m.asEntityCursor = 0
		case m.asBoxCursor-len(s.runs)-len(s.teams) < len(s.jobs):
			// Descending from a job row keeps THAT job focused in the entity list.
			m.asEntitySrc = asRailRef{jobs: true}
			m.asEntityCursor = m.asBoxCursor - len(s.runs) - len(s.teams)
		default:
			return m, nil
		}
		m.asMode = asModeEntity
		m.asCardScroll = 0
	}
	if m.asMode == asModeEntity {
		return m.focusEntityIO()
	}
	return m, nil
}

// jobCardStale reports whether the visible job card needs a fresh io load: a different
// job, a still-running one (its Activity feed climbs), or the first refresh that sees it
// terminal (the final .answer lands after the running→done flip).
func (m Model) jobCardStale(j subagent.Result) bool {
	return j.JobID != m.asDetailJobID || !isTerminalLeaf(j.Status) || !m.asDetailTerminal
}

// focusEntityIO loads the focused entity's detail payload per the collection kind (job io
// vs teammate inbox/transcript). Called whenever the entity cursor lands on a row.
func (m Model) focusEntityIO() (tea.Model, tea.Cmd) {
	if m.asEntitySrc.jobs {
		return m.focusJobIO()
	}
	return m.focusMateIO()
}

// focusJobIO resets the card scroll/expand state and loads the focused job's io + activity
// into the inline detail, nonce-gated (mirror focusLeafIO). Called whenever the entity
// cursor lands on a job.
func (m Model) focusJobIO() (tea.Model, tea.Cmd) {
	m.asCardScroll = 0
	m.asPromptExpanded = false
	j, ok := m.selectedJob()
	if !ok {
		m.asDetailJobID, m.asDetailPrompt, m.asDetailAnswer, m.asDetailIO = "", "", "", false
		m.asDetailSnap = activitySnapshot{}
		return m, nil
	}
	m.asDetailNonce++
	m.asDetailTerminal = isTerminalLeaf(j.Status)
	return m, loadJobIOCmd(j.JobID, m.asDetailNonce, m.boardEpoch)
}

// focusMateIO is focusJobIO's teammate sibling: reset the card scroll/expand state and load
// the focused teammate's inbox + transcript projection, on the same nonce gate.
func (m Model) focusMateIO() (tea.Model, tea.Cmd) {
	m.asCardScroll = 0
	m.asPromptExpanded = false
	t, ok := m.selectedTeammate()
	if !ok {
		m.asMateKey, m.asMateSnap, m.asMateFound = "", teammateSnapshot{}, false
		return m, nil
	}
	m.asDetailNonce++
	return m, loadMateCmd(t, m.sessionMeta[t.LeadSessionID].Cwd, m.asDetailNonce, m.boardEpoch)
}

// asAscend climbs one level: entity → boxes, boxes → sessions (multi-session project) or
// projects (multi-project), sessions → projects. At the board's TOP level there's nowhere
// to climb: exitAtTop leaves for Vendors (esc) vs stays put (←) — so repeated ← can't fall
// out of the board (mirror wfAscend).
func (m Model) asAscend(exitAtTop bool) (tea.Model, tea.Cmd) {
	projects := m.asProjects()
	switch m.asMode {
	case asModeEntity:
		// A single-kind session skipped the boxes level on the way down; skip it on the
		// way up too.
		if s, ok := m.focusedSession(); ok {
			if _, single := singleKindSrc(s); !single {
				m.asMode = asModeBoxes
				return m, nil
			}
		} else {
			m.asMode = asModeBoxes
			return m, nil
		}
		fallthrough
	case asModeBoxes:
		if p, ok := m.focusedProjectGroup(); ok && len(p.sessions) > 1 {
			m.asMode = asModeSessions
			return m, nil
		}
		if len(projects) > 1 {
			m.asMode = asModeProjects
			return m, nil
		}
	case asModeSessions:
		if len(projects) > 1 {
			m.asMode = asModeProjects
			return m, nil
		}
	}
	if exitAtTop {
		return m.toList()
	}
	return m, nil
}

// asSessions is the board's session tree — recomputed per call off the loaded slices.
func (m Model) asSessions() []asSession {
	return groupSessions(m.teammates, m.jobs, m.wfGroups())
}

// asProjects is the session tree bucketed by recorded working directory.
func (m Model) asProjects() []asProject { return groupProjects(m.asSessions(), m.sessionMeta) }

// focusedProjectGroup returns the project the sessions/boxes levels are rooted on.
func (m Model) focusedProjectGroup() (asProject, bool) {
	if m.focusedProject == asNoFocus {
		return asProject{}, false
	}
	for _, p := range m.asProjects() {
		if p.dir == m.focusedProject {
			return p, true
		}
	}
	return asProject{}, false
}

// focusedSession returns the session the boxes/entity levels are rooted on; ok=false when
// nothing is focused yet or its session vanished.
func (m Model) focusedSession() (asSession, bool) {
	if m.focusedSessionID == asNoFocus {
		return asSession{}, false
	}
	for _, s := range m.asSessions() {
		if s.sessionID == m.focusedSessionID {
			return s, true
		}
	}
	return asSession{}, false
}

// railEntities returns the L3 collection's rows per asEntitySrc: a team's members (jobs
// nil) or the session's standalone jobs (members nil).
func (m Model) railEntities() (members []teardown.Teammate, jobs []subagent.Result) {
	s, ok := m.focusedSession()
	if !ok {
		return nil, nil
	}
	if m.asEntitySrc.jobs {
		return nil, s.jobs
	}
	for _, t := range s.teams {
		if t.name == m.asEntitySrc.team {
			return t.members, nil
		}
	}
	return nil, nil
}

// selectedTeammate returns the teammate under asEntityCursor (ok=false on the jobs rail).
func (m Model) selectedTeammate() (teardown.Teammate, bool) {
	members, _ := m.railEntities()
	if m.asEntityCursor < 0 || m.asEntityCursor >= len(members) {
		return teardown.Teammate{}, false
	}
	return members[m.asEntityCursor], true
}

// selectedJob returns the job under asEntityCursor (ok=false on a team rail).
func (m Model) selectedJob() (subagent.Result, bool) {
	_, jobs := m.railEntities()
	if m.asEntityCursor < 0 || m.asEntityCursor >= len(jobs) {
		return subagent.Result{}, false
	}
	return jobs[m.asEntityCursor], true
}

// asNoFocus marks "no project/session focused" — "" can't serve as the sentinel because it
// is the real id of the "(no session)" / "(no project)" buckets. The control byte keeps it
// impossible as a real value; it never renders.
const asNoFocus = "\x00unfocused"

// rerootSpawn re-derives the board's focus after a refresh lands (mirror rerootWorkflows):
// a still-valid focus chain keeps its drill state; a broken one (a fresh entry parks on
// asNoFocus so the first load routes, or the focused project/session vanished) re-routes by
// the loaded counts — 0 sessions → empty boxes, 1 session → enterSession (its boxes, or its
// detail view when single-kind), 1 project → its session list, >1 project → the project
// list.
func (m *Model) rerootSpawn() {
	sessions := m.asSessions()
	projects := groupProjects(sessions, m.sessionMeta)

	switch m.asMode {
	case asModeRunPhases, asModeRunAgent:
		// The run drill keeps its place while the run and its session both survive; a
		// vanished run demotes to the session's boxes, a vanished session re-routes.
		if _, ok := m.focusedGroup(); ok {
			if s, ok := m.focusedSession(); ok {
				m.focusedProject = sessionProjectDir(s, m.sessionMeta)
				m.clampRunCursors()
				m.clampAsCursors()
				return
			}
		}
		if s, ok := m.focusedSession(); ok {
			m.asMode = asModeBoxes
			m.focusedRunID = ""
			m.focusedProject = sessionProjectDir(s, m.sessionMeta)
			m.clampAsCursors()
			return
		}
	case asModeEntity:
		if s, ok := m.focusedSession(); ok {
			// The session's recorded cwd can resolve late; keep the project link true.
			m.focusedProject = sessionProjectDir(s, m.sessionMeta)
			m.clampAsCursors()
			// The focused collection can vanish mid-watch (clamp demotes to boxes); a
			// now-single-kind session skips that level, as descend would.
			if m.asMode == asModeBoxes {
				if _, single := singleKindSrc(s); single {
					m.enterSession(s)
					m.clampAsCursors()
				}
			}
			return
		}
	case asModeBoxes:
		if s, ok := m.focusedSession(); ok {
			m.focusedProject = sessionProjectDir(s, m.sessionMeta)
			// The boxes level can turn degenerate mid-watch (the session lost its last
			// team or its last job): skip to the detail view, as descend would.
			if _, single := singleKindSrc(s); single {
				m.enterSession(s)
			}
			m.clampAsCursors()
			return
		}
	case asModeSessions:
		if p, ok := m.focusedProjectGroup(); ok && len(p.sessions) > 1 {
			m.clampAsCursors()
			return
		}
	case asModeProjects:
		if len(projects) > 1 {
			m.clampAsCursors()
			return
		}
	}

	m.asProjectCursor, m.asSessionCursor, m.asBoxCursor, m.asEntityCursor, m.asCardScroll = 0, 0, 0, 0, 0
	m.focusedProject, m.focusedSessionID = asNoFocus, asNoFocus
	switch {
	case len(sessions) == 0:
		m.asMode = asModeBoxes // renders the empty state
	case len(sessions) == 1:
		m.focusedProject = projects[0].dir
		m.enterSession(sessions[0])
	case len(projects) == 1:
		m.focusedProject = projects[0].dir
		m.asMode = asModeSessions
	default:
		m.asMode = asModeProjects
	}
	m.clampAsCursors()
}

// clampAsCursors bounds every level's cursor to the live tree and drops out of entity mode
// when its collection emptied, so render can never index past the end.
func (m *Model) clampAsCursors() {
	m.asProjectCursor = clampIndex(m.asProjectCursor, len(m.asProjects()))
	if p, ok := m.focusedProjectGroup(); ok {
		m.asSessionCursor = clampIndex(m.asSessionCursor, len(p.sessions))
	} else {
		m.asSessionCursor = 0
	}
	s, ok := m.focusedSession()
	if !ok {
		m.asBoxCursor, m.asEntityCursor = 0, 0
		if m.asMode == asModeEntity {
			m.asMode = asModeBoxes
		}
		return
	}
	m.asBoxCursor = clampIndex(m.asBoxCursor, len(s.runs)+len(s.teams)+len(s.jobs))
	members, jobs := m.railEntities()
	n := len(members) + len(jobs)
	m.asEntityCursor = clampIndex(m.asEntityCursor, n)
	if m.asMode == asModeEntity && n == 0 {
		m.asMode = asModeBoxes
	}
}

// The L2 continuum walks the session's runs, then its teams, then its jobs.

// boxRun returns the run under the L2 continuum cursor (ok=false off the runs range).
func (m Model) boxRun() (runGroup, bool) {
	s, ok := m.focusedSession()
	if !ok {
		return runGroup{}, false
	}
	if i := m.asBoxCursor; i >= 0 && i < len(s.runs) {
		return s.runs[i], true
	}
	return runGroup{}, false
}

// boxTeam returns the team name under the L2 continuum cursor (ok=false off the teams range).
func (m Model) boxTeam() (string, bool) {
	t, ok := m.boxTeamGroup()
	return t.name, ok
}

// boxTeamGroup returns the team under the L2 continuum cursor (ok=false off the teams range).
func (m Model) boxTeamGroup() (asTeam, bool) {
	s, ok := m.focusedSession()
	if !ok {
		return asTeam{}, false
	}
	if i := m.asBoxCursor - len(s.runs); i >= 0 && i < len(s.teams) {
		return s.teams[i], true
	}
	return asTeam{}, false
}

// isEndedTeam reports whether a team is rendered from a history record (gone from
// live discovery) — endedSeen carries exactly those teams.
func (m Model) isEndedTeam(name string) bool {
	_, ok := m.endedSeen[name]
	return ok
}

// boxPreviewJob is the job whose card the Subagents box shows: the cursored job while the
// continuum sits in the jobs range, else the FIRST job — the box never shows a flat list.
func (m Model) boxPreviewJob() (subagent.Result, bool) {
	if j, ok := m.boxJob(); ok {
		return j, true
	}
	s, ok := m.focusedSession()
	if !ok || len(s.jobs) == 0 {
		return subagent.Result{}, false
	}
	return s.jobs[0], true
}

// boxJob returns the job under the L2 continuum cursor (ok=false off the jobs range).
func (m Model) boxJob() (subagent.Result, bool) {
	s, ok := m.focusedSession()
	if !ok {
		return subagent.Result{}, false
	}
	if i := m.asBoxCursor - len(s.runs) - len(s.teams); i >= 0 && i < len(s.jobs) {
		return s.jobs[i], true
	}
	return subagent.Result{}, false
}

// boxRowRef returns the typed identity of the L2 row under asBoxCursor.
func (m Model) boxRowRef() (asBoxRef, bool) {
	s, ok := m.focusedSession()
	if !ok {
		return asBoxRef{}, false
	}
	switch {
	case m.asBoxCursor < len(s.runs):
		return asBoxRef{runID: s.runs[m.asBoxCursor].runID}, true
	case m.asBoxCursor-len(s.runs) < len(s.teams):
		return asBoxRef{isTeam: true, team: s.teams[m.asBoxCursor-len(s.runs)].name}, true
	case m.asBoxCursor-len(s.runs)-len(s.teams) < len(s.jobs):
		return asBoxRef{jobID: s.jobs[m.asBoxCursor-len(s.runs)-len(s.teams)].JobID}, true
	}
	return asBoxRef{}, false
}

// reanchorBox re-finds the previously cursored L2 row by its typed identity after a
// refresh; a vanished row leaves the index-clamped cursor in place.
func (m *Model) reanchorBox(prev asBoxRef) {
	s, ok := m.focusedSession()
	if !ok {
		return
	}
	if prev.runID != "" {
		for i, g := range s.runs {
			if g.runID == prev.runID {
				m.asBoxCursor = i
				return
			}
		}
		return
	}
	if prev.isTeam {
		for i, t := range s.teams {
			if t.name == prev.team {
				m.asBoxCursor = len(s.runs) + i
				return
			}
		}
		return
	}
	for i, j := range s.jobs {
		if prev.jobID != "" && j.JobID == prev.jobID {
			m.asBoxCursor = len(s.runs) + len(s.teams) + i
			return
		}
	}
}

// clampAsCardScroll bounds the visible card's scroll to [0, lines-viewport] so j/k never
// scroll past the content (mirror clampCardScroll). The card lives in the entity view OR
// inline in the L2 Subagents box — each with its own geometry.
func (m Model) clampAsCardScroll(v int) int {
	var max int
	if m.asMode == asModeBoxes {
		if j, ok := m.boxJob(); ok {
			s, _ := m.focusedSession()
			_, _, jobsH := m.splitBoxHeights(s)
			max = len(m.jobDetailLines(j, m.jobsBoxRightWidth(s))) - jobsH
		}
	} else {
		max = len(m.entityDetailLines(m.asEntityRightWidth())) - m.asEntityBodyHeight()
	}
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

// wfGroups is the board's run→phase→agent tree (newest-first) — the single source the Dynamic
// Workflows box rail, phases, agents, and their cursors index.
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

// selectedRunID returns the focused run id (x/r act on the whole run, even when the focused phase
// has no agents).
func (m Model) selectedRunID() (string, bool) {
	if _, ok := m.focusedGroup(); ok {
		return m.focusedRunID, true
	}
	return "", false
}

// clampRunCursors bounds the run drill's phase/agent cursors to the live data and drops out
// of the agent level when the focused phase has no agents, so Enter/render can never index
// past the end.
func (m *Model) clampRunCursors() {
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
	if m.asMode == asModeRunAgent && agents == 0 {
		m.asMode = asModeRunPhases
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

// teamHistCtlMsg carries the outcome of an ended-team record delete. Owned by
// screenSpawn; epoch is the originating board visit so a stale result is dropped.
type teamHistCtlMsg struct {
	team  string
	err   error
	epoch int
}

func (teamHistCtlMsg) owningScreen() screen { return screenSpawn }

// deleteTeamHistCmd removes an ended team's history record off the Update goroutine,
// reporting the outcome via its teamHistCtlMsg (mirror deleteRunCmd).
func deleteTeamHistCmd(team string, epoch int) tea.Cmd {
	return func() tea.Msg {
		return teamHistCtlMsg{team: team, err: teamhist.Delete(team), epoch: epoch}
	}
}

// updateWfPhases (run drill): ↑/↓ walk phases, → descend into a non-empty phase's agents, ←/esc
// ascend to the session's boxes; x/r control the focused run.
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
	case "esc", "left":
		return m.wfAscend()
	case "x":
		return m.stopFocusedPhase()
	case "r":
		return m.restartFocusedPhase()
	default:
		return m.wfControl(msg)
	}
	return m, nil
}

// updateWfAgent (run drill): ↑/↓ walk agents (reloading the inline detail), j/k scroll that detail,
// ←/esc ascend to Phases; r restarts ONLY the focused agent; x stops/holds ONLY the focused live agent.
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
		return m.wfAscend()
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
	case "x":
		return m.stopFocusedLeaf()
	default:
		return m.wfControl(msg)
	}
	return m, nil
}

// wfDescend drops Phases → agent detail (when the focused phase has agents, loading the
// first agent's io). The agent level is the deepest.
func (m Model) wfDescend() (tea.Model, tea.Cmd) {
	if p, ok := m.focusedPhase(); ok && len(p.jobs) > 0 {
		m.asMode = asModeRunAgent
		m.wfAgentCursor = 0
		return m.focusLeafIO()
	}
	return m, nil
}

// wfAscend climbs out of the run drill: agent → Phases, Phases → the session's boxes
// (which always exist for a session with runs — the skip rule guarantees it).
func (m Model) wfAscend() (tea.Model, tea.Cmd) {
	if m.asMode == asModeRunAgent {
		m.asMode = asModeRunPhases
		return m, nil
	}
	m.asMode = asModeBoxes
	m.focusedRunID = ""
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
	return m, loadLeafIOCmd(job, m.wfDetailNonce, m.boardEpoch)
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

// wfBusy reports whether a stop/restart/delete is already in flight for runID — the in-flight guard
// that makes a second x/r/d on the same run a no-op until its workflowCtlMsg lands.
func (m Model) wfBusy(runID string) bool { return m.wfRestarting[runID] }

// markBusy adds runID to the in-flight guard (lazily creating the map).
func (m Model) markBusy(runID string) Model {
	if m.wfRestarting == nil {
		m.wfRestarting = map[string]bool{}
	}
	m.wfRestarting[runID] = true
	return m
}

// restartFocusedLeaf opens a confirm for restarting just the focused agent (its journal key scopes
// the restart to that leaf); runConfirmed dispatches it and holds the modal until the outcome lands.
func (m Model) restartFocusedLeaf() (tea.Model, tea.Cmd) {
	job, ok := m.selectedLeaf()
	if !ok {
		return m, nil
	}
	g, gok := m.focusedGroup()
	if !gok {
		return m, nil
	}
	if m.wfBusy(g.runID) {
		return m, nil
	}
	// On a LIVE run a nonterminal leaf restarts IN PLACE over the control plane (held
	// wakes; running/queued kills the attempt and re-runs); a CONSUMED leaf
	// (done/failed/cached) can only ride the honest whole-run composite — its value
	// already flowed into the script.
	if g.status == "running" {
		switch job.Status {
		case "held":
			m.confirm = &confirmModal{kind: confirmRestartLeaf, id: g.runID, arg: job.JobID,
				prompt: "Restart this held agent (re-runs it in place)?"}
			return m, nil
		case "running", "queued", "":
			m.confirm = &confirmModal{kind: confirmRestartLeaf, id: g.runID, arg: job.JobID,
				prompt: "Restart just this agent (cancels its current attempt and re-runs it in place)?"}
			return m, nil
		default:
			m.confirm = &confirmModal{kind: confirmRestartAgent, id: g.runID, arg: job.JournalKey,
				prompt: "Restart the whole run from this agent (re-runs in-flight siblings)?"}
			return m, nil
		}
	}
	m.confirm = &confirmModal{kind: confirmRestartAgent, id: g.runID, arg: job.JournalKey,
		prompt: "Restart just this agent?"}
	return m, nil
}

// stopFocusedPhase is the Phases pane's x — scoped to the focused phase title on a
// LIVE run: every live member holds and future members of the title park (a reused
// title is ONE merged target). A finished run has nothing to stop.
func (m Model) stopFocusedPhase() (tea.Model, tea.Cmd) {
	p, ok := m.focusedPhase()
	if !ok {
		return m, nil
	}
	g, gok := m.focusedGroup()
	if !gok {
		return m, nil
	}
	if m.wfBusy(g.runID) {
		return m, nil
	}
	if g.status != "running" {
		return m.withInfo("the run is not live — nothing to stop", false), nil
	}
	m.confirm = &confirmModal{kind: confirmStopPhase, id: g.runID, arg: p.title,
		prompt: fmt.Sprintf("Stop phase %q? Its agents hold until you restart the phase (one title = one merged target); everything else keeps running.", p.title)}
	return m, nil
}

// restartFocusedPhase is the Phases pane's r: on a LIVE run it re-runs the title's held
// and running members in place (finished members keep their results); on a finished run
// it is the keyed phase restart, with shared-key widening named in the prompt.
func (m Model) restartFocusedPhase() (tea.Model, tea.Cmd) {
	p, ok := m.focusedPhase()
	if !ok {
		return m, nil
	}
	g, gok := m.focusedGroup()
	if !gok {
		return m, nil
	}
	if m.wfBusy(g.runID) {
		return m, nil
	}
	if g.status == "running" {
		m.confirm = &confirmModal{kind: confirmRestartPhase, id: g.runID, arg: p.title,
			prompt: fmt.Sprintf("Restart phase %q? Its held and running agents re-run in place; finished agents keep their results.", p.title)}
		return m, nil
	}
	prompt := fmt.Sprintf("Re-run phase %q from the journal?", p.title)
	if widened := m.phaseWidening(g, p.title); len(widened) > 0 {
		prompt += fmt.Sprintf(" Shared agents widen the re-run into phase(s) %s.", strings.Join(widened, ", "))
	}
	m.confirm = &confirmModal{kind: confirmRestartPhaseKeyed, id: g.runID, arg: p.title, prompt: prompt}
	return m, nil
}

// phaseWidening names the OTHER phase titles a keyed phase restart would re-run too:
// identical agent() calls share one content key, and the restart drops whole keys.
func (m Model) phaseWidening(g runGroup, phase string) []string {
	keys := map[string]bool{}
	for _, p := range g.phases {
		if p.title != phase {
			continue
		}
		for _, j := range p.jobs {
			if j.JournalKey != "" {
				keys[j.JournalKey] = true
			}
		}
	}
	widened := map[string]bool{}
	for _, p := range g.phases {
		if p.title == phase {
			continue
		}
		for _, j := range p.jobs {
			if keys[j.JournalKey] && j.JournalKey != "" {
				widened[p.title] = true
			}
		}
	}
	out := make([]string, 0, len(widened))
	for t := range widened {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// stopFocusedLeaf is the agent pane's x — scoped to exactly the focused leaf. A live
// (running/queued) leaf gets the kill-and-HOLD confirm; held is already stopped; a
// terminal leaf has nothing left to stop (its value is consumed).
func (m Model) stopFocusedLeaf() (tea.Model, tea.Cmd) {
	job, ok := m.selectedLeaf()
	if !ok {
		return m, nil
	}
	g, gok := m.focusedGroup()
	if !gok {
		return m, nil
	}
	if m.wfBusy(g.runID) {
		return m, nil
	}
	if g.status != "running" {
		return m.withInfo("the run is not live — nothing to stop", false), nil
	}
	switch job.Status {
	case "held":
		return m.withInfo("agent is already held — r restarts it", false), nil
	case "running", "queued", "":
		m.confirm = &confirmModal{kind: confirmStopLeaf, id: g.runID, arg: job.JobID,
			prompt: "Stop just this agent? Its part of the script waits until you restart it; everything else keeps running."}
		return m, nil
	default:
		return m.withInfo("agent already finished — r offers the whole-run restart", false), nil
	}
}

// wfControl runs the run-level controls (x stop / r restart) shared by the Phases pane and the Agent
// pane's non-r keys — both target the FOCUSED run, so they work even when the focused phase has no
// agents. (Save lives at the outermost run row, not in the drill — see updateAsBoxes.)
func (m Model) wfControl(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	runID, ok := m.selectedRunID()
	if !ok {
		return m, nil
	}
	switch msg.String() {
	case "x":
		if m.wfBusy(runID) {
			return m, nil
		}
		// There is no per-agent stop — x always stops the WHOLE run; say so plainly (the agent
		// pane makes it look agent-scoped).
		return m.openConfirm(confirmStop, runID, "Stop the whole run (all agents)?")
	case "r":
		if m.wfBusy(runID) {
			return m, nil
		}
		return m.openConfirm(confirmRestart, runID, "Restart this run and re-run its agents?")
	}
	return m, nil
}

// startSaveWorkflow opens the centered name prompt to save runGroup g as a named, reusable workflow
// (prefilled with the run's name) and pins g as the save target. updateWfSaveInput then handles
// enter (save) / esc (cancel).
func (m Model) startSaveWorkflow(g runGroup) (tea.Model, tea.Cmd) {
	m.wfSaving = true
	m.wfSaveRun = g
	m.wfSaveInput = newTextInput(g.name, "workflow name", false)
	m.wfSaveInput.Focus()
	return m, textinput.Blink
}

// updateWfSaveInput drives the save-workflow name prompt: enter saves the run pinned at open time
// (wfSaveRun — a refresh-reanchored cursor must not retarget the save; a since-vanished run just
// fails the save itself) under the typed name, a blank name cancels, esc cancels, any other key
// edits the input.
func (m Model) updateWfSaveInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.wfSaving = false
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.wfSaveInput.Value())
		g := m.wfSaveRun
		m.wfSaving = false
		if name == "" {
			return m, nil
		}
		return m, saveWorkflowCmd(g.runID, name, g.sessionID, g.description, m.boardEpoch)
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

// sessionDelMsg carries the outcome of a whole-session wipe. Owned by screenSpawn; epoch gates a
// stale result from a prior board visit.
type sessionDelMsg struct {
	removed int
	teams   int
	err     error
	epoch   int
}

func (sessionDelMsg) owningScreen() screen { return screenSpawn }

// deleteSessionCmd wipes every record in a session off the Update goroutine (PurgeRun can block on an
// engine stop). It snapshots pins and fails closed — a pin read error aborts rather than deleting
// pin-blind — then removes the session's runs/jobs and ended-team history.
func deleteSessionCmd(sessionID string, epoch int) tea.Cmd {
	return func() tea.Msg {
		pins, perr := pinned.Snapshot()
		if perr != nil {
			return sessionDelMsg{err: perr, epoch: epoch}
		}
		removed, derr := subagent.DeleteSession(sessionID, pins)
		teams, eerr := teamhist.ClearEnded(sessionID, pins)
		err := derr
		if err == nil {
			err = eerr
		}
		return sessionDelMsg{removed: removed, teams: teams, err: err, epoch: epoch}
	}
}

// addItem is one selectable row of the grouped add picker: a provider class plus
// the seed it implies (a Templates index, an OAITemplates index, or neither for a
// Custom / codex row).
type addItem struct {
	label string
	class addCategory
	tIdx  int // Templates index, or -1
	oIdx  int // OAITemplates index, or -1
}

type addGroup struct {
	header string
	items  []addItem
}

// addGroups builds the grouped add picker: every provider class with its seeds
// under one header, so a provider is chosen in a single screen.
func addGroups() []addGroup {
	a := addGroup{header: "Anthropic-protocol API  (DeepSeek / GLM / Kimi / Qwen / …)"}
	for i := range Templates {
		a.items = append(a.items, addItem{Templates[i].Label, addCatAnthropic, i, -1})
	}
	a.items = append(a.items, addItem{"Custom (fill everything manually)", addCatAnthropic, -1, -1})

	o := addGroup{header: "OpenAI-protocol API  (OpenAI / Groq / Together / vLLM …)"}
	for i := range OAITemplates {
		o.items = append(o.items, addItem{OAITemplates[i].Label, addCatOpenAI, -1, i})
	}
	o.items = append(o.items, addItem{"Custom OpenAI-compatible (fill everything manually)", addCatOpenAI, -1, -1})

	c := addGroup{header: "CLI auth  (reuse a ChatGPT subscription via codex)"}
	c.items = append(c.items, addItem{"Codex", addCatCLI, -1, -1})

	return []addGroup{a, o, c}
}

// addItems flattens the groups to the selectable rows the cursor walks.
func addItems() []addItem {
	var xs []addItem
	for _, g := range addGroups() {
		xs = append(xs, g.items...)
	}
	return xs
}

// updatePickTemplate drives the single grouped add picker: one cursor over every
// provider across all three classes; enter/→ opens the chosen class's add form.
func (m Model) updatePickTemplate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := addItems()
	switch msg.String() {
	case "esc", "q", "left":
		return m.toList()
	case "up", "k":
		if m.tmplCursor > 0 {
			m.tmplCursor--
		}
	case "down", "j":
		if m.tmplCursor < len(items)-1 {
			m.tmplCursor++
		}
	case "enter", "right":
		if m.tmplCursor >= 0 && m.tmplCursor < len(items) {
			return m.chooseAddItem(items[m.tmplCursor])
		}
	}
	return m, nil
}

// uniqueName returns base, or the first free "<base>-N" when base is already a
// configured provider — a prefill convenience for adding a second provider of one
// type. The real uniqueness guard stays addLocked's VENDOR_EXISTS check; a blank
// base (a Custom entry) is returned unchanged.
func (m Model) uniqueName(base string) string {
	if base == "" {
		return ""
	}
	taken := make(map[string]bool, len(m.vendors))
	for _, v := range m.vendors {
		taken[v.Name] = true
	}
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		if cand := fmt.Sprintf("%s-%d", base, n); !taken[cand] {
			return cand
		}
	}
}

// chooseAddItem opens the add form (or the codex auth flow) for a picked row.
func (m Model) chooseAddItem(it addItem) (tea.Model, tea.Cmd) {
	switch it.class {
	case addCatCLI:
		return m.enterCodexFlow()
	case addCatOpenAI:
		var t OAITemplate
		protocol := config.ProtocolOpenAIChat // a Custom entry defaults to Chat
		if it.oIdx >= 0 {
			t = OAITemplates[it.oIdx]
			protocol = t.Protocol
		}
		t.Name = m.uniqueName(t.Name)
		m.form = newOpenAIAddForm(t)
		m.addProtocol = protocol
	default: // addCatAnthropic
		var t Template // zero value = Custom (blank fields)
		if it.tIdx >= 0 {
			t = Templates[it.tIdx]
		}
		t.Name = m.uniqueName(t.Name)
		m.form = newAddForm(t)
		m.addProtocol = ""
	}
	m.formMode = modeAdd
	m.screen = screenForm
	return m, textinput.Blink
}

// enterCodexFlow opens the codex add form. The credential the provider will use —
// the cli-ride-capable default for the first codex, a fresh per-provider login for
// any later one — is decided here only to label the source note; the actual
// login (when the credential has none) happens at submit, keyed on the final name.
func (m Model) enterCodexFlow() (tea.Model, tea.Cmd) {
	var note string
	if m.codexDefaultTaken() {
		note = "a separate codex login is created for this provider when you submit"
	} else {
		note = codexSourceNote(codexproxy.StatusReport(codexproxy.SecretRef))
	}
	m.form = newCodexAddForm(m.uniqueName("codex"), codexproxy.ScanDefaultModel("gpt-5.5"), note)
	m.formMode = modeAdd
	m.addProtocol = config.ProtocolCodexOAuth
	m.screen = screenForm
	return m, textinput.Blink
}

// codexDefaultTaken reports whether an existing codex provider already claims the
// default (cli-ride-capable) credential, so a new codex must get its own login.
func (m Model) codexDefaultTaken() bool {
	for _, v := range m.vendors {
		if v.Protocol == config.ProtocolCodexOAuth && codexproxy.IsDefaultCredentialRef(v.SecretRef) {
			return true
		}
	}
	return false
}

// vendorClassRank and vendorClassHeader group the providers list by wire class:
// Anthropic-protocol (0) < OpenAI-protocol (1) < CLI auth (2).
func vendorClassRank(protocol string) int {
	switch protocol {
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses:
		return 1
	case config.ProtocolCodexOAuth:
		return 2
	default:
		return 0
	}
}

func vendorClassHeader(protocol string) string {
	switch protocol {
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses:
		return "OpenAI-protocol"
	case config.ProtocolCodexOAuth:
		return "CLI auth"
	default:
		return "Anthropic-protocol"
	}
}

// codexStaticModelList wraps the static codex model ids as the picker's model
// list (codex has no live models endpoint to probe at add time).
func codexStaticModelList() []models.Model {
	ids := codexproxy.StaticModels()
	out := make([]models.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, models.Model{ID: id})
	}
	return out
}

// codexSourceNote names the login a codex provider will use, shown on the add form.
func codexSourceNote(st codexproxy.CredStatus) string {
	switch st.Active {
	case "cli-ride":
		return "reuses the codex CLI login (account " + st.Account + ") — no key needed; if it stops working, run: cc-fleet codex login"
	case "own":
		return "uses cc-fleet's own codex login (account " + st.Account + ")"
	default:
		return ""
	}
}

// updateCodexAuth drives the codex CLI-auth screen: an optional source choice (reuse
// a detected subscription — committing the add directly — or log in separately), a
// consent gate, then a device-code login polled on its own interval until authorized.
func (m Model) updateCodexAuth(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.codexAuthStage {
	case codexAuthChooseSource:
		switch msg.String() {
		case "esc", "q", "left":
			// Back to the filled-in codex form (still in m.form) to amend or abandon.
			m.codexPendingAdd = nil
			m.screen = screenForm
			return m, textinput.Blink
		case "up", "k":
			if m.codexSourceCursor > 0 {
				m.codexSourceCursor--
			}
		case "down", "j":
			if m.codexSourceCursor < 1 {
				m.codexSourceCursor++
			}
		case "enter":
			// Commit the cursored choice: reuse rides ~/.codex (no token write, no
			// rollback); separate login commits the row first and marks it for the
			// login — same ordering as the no-source path. Either way the committing
			// stage traps keys until opDoneMsg routes on.
			if m.codexPendingAdd == nil {
				return m.toList()
			}
			req := *m.codexPendingAdd
			m.codexPendingAdd = nil
			if m.codexSourceCursor == 1 {
				m.codexCommittedName = req.Name
			}
			m.codexAuthStage = codexAuthCommitting
			m.loading = true
			return m, addVendorCmd(req)
		}
	case codexAuthConsent:
		switch msg.String() {
		case "esc", "q", "left", "n":
			// The row was committed before this login (see submitAddCodex) — abandoning
			// the login rolls it back.
			return m.codexAbandonLogin()
		case "enter", "y":
			if m.codexAuthCancel != nil {
				m.codexAuthCancel() // tear down any prior session before starting a fresh one
			}
			m.codexAuthStage = codexAuthDevice
			m.codexAuthErr = ""
			m.loading = true
			m.codexAuthEpoch++ // a fresh login attempt; a prior visit's in-flight msgs fail the gate
			ctx, cancel := context.WithCancel(context.Background())
			m.codexAuthCtx, m.codexAuthCancel = ctx, cancel
			return m, beginCodexAuthCmd(ctx, m.codexAuthEpoch, m.codexAddRef)
		}
	case codexAuthDevice:
		if s := msg.String(); s == "esc" || s == "q" {
			return m.codexAbandonLogin()
		}
	case codexAuthCommitting:
		// The add is committing (a short flocked local write); every key is trapped so
		// the underlying form can't change until opDoneMsg routes to consent or result.
		return m, nil
	}
	return m, nil
}

// codexAbandonLogin backs out of a codex login the user cancelled. It cancels the
// session (so an in-flight poll cannot persist a token after this) and bumps the epoch
// (so any in-flight begin/poll/tick result is dropped). A row committed ahead of the
// login is rolled back so the cancelled add leaves nothing behind; with no committed
// row it just returns to the list.
func (m Model) codexAbandonLogin() (tea.Model, tea.Cmd) {
	if m.codexAuthCancel != nil {
		m.codexAuthCancel()
		m.codexAuthCancel, m.codexAuthCtx = nil, nil
	}
	m.codexAuthEpoch++
	m.codexAuth = nil
	name := m.codexCommittedName
	m.codexCommittedName = ""
	if name == "" {
		m.loading = false
		return m.toList()
	}
	m.loading = true
	return m, codexRollbackCmd(name, m.codexAuthEpoch)
}

// codexAuthBegunMsg carries a started device-code session (URL + code to show);
// codexAuthPollMsg an authorization poll result; codexAuthTickMsg the interval to
// poll again. All carry the login attempt's epoch and are owned by screenCodexAuth,
// so a result from a prior visit (esc then re-enter) or from after the user left
// the screen is dropped.
type codexAuthBegunMsg struct {
	epoch   int
	session *codexproxy.LoginSession
	err     error
}

func (codexAuthBegunMsg) owningScreen() screen { return screenCodexAuth }

type codexAuthPollMsg struct {
	epoch int
	done  bool
	err   error
}

func (codexAuthPollMsg) owningScreen() screen { return screenCodexAuth }

type codexAuthTickMsg struct{ epoch int }

func (codexAuthTickMsg) owningScreen() screen { return screenCodexAuth }

// beginCodexAuthCmd and pollCodexAuthCmd derive their per-call timeout from the
// session context, so codexAbandonLogin's cancel aborts an in-flight begin/poll —
// the poll then returns before persisting a token, leaving no orphan login.
func beginCodexAuthCmd(ctx context.Context, epoch int, ref string) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		s, err := codexproxy.BeginDeviceLogin(cctx, ref)
		return codexAuthBegunMsg{epoch: epoch, session: s, err: err}
	}
}

func pollCodexAuthCmd(ctx context.Context, epoch int, s *codexproxy.LoginSession) tea.Cmd {
	return func() tea.Msg {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		done, err := s.Poll(cctx)
		return codexAuthPollMsg{epoch: epoch, done: done, err: err}
	}
}

func codexAuthTickCmd(epoch int, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return codexAuthTickMsg{epoch: epoch} })
}

func (m Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "esc" {
		return m.toList()
	}
	// ← returns to the provider list from a row that doesn't consume it — a text
	// field keeps the arrow for its input cursor, a choice field cycles options,
	// and a 1M toggle uses ← to return to its model slot.
	if msg.String() == "left" && !m.form.focusedText() && !m.form.focusedChoice() && !m.form.focusedOneM() {
		return m.toList()
	}
	// Enter (or →, the descend key) on the "Manage API keys →" action row (edit
	// form only) opens the per-vendor key manager and loads its key set. Not while a
	// submit is in flight: a key confirm opened in that window would swallow the
	// pending outcome modal (withInfo never clobbers an open modal).
	if (msg.String() == "enter" || msg.String() == "right") && m.formMode == modeEdit &&
		m.form.focusedKey() == "manage_keys" && !m.loading {
		m.screen = screenKeys
		m.keyVendor = m.editName
		m.keyCursor = 0
		m.keyEditing = false
		m.keyErr = ""
		m.keys = nil
		return m, loadKeysetCmd(m.editName)
	}
	// Enter on the Default model field opens the model picker ("pick, don't type").
	// codex has no probeable models endpoint (its /v1/models is the lazily-started
	// loopback daemon), so it seeds the picker from the static codex model list
	// instead of a fetch — for both add and edit.
	if msg.String() == "enter" && isModelSlotKey(m.form.focusedKey()) &&
		m.addProtocol == config.ProtocolCodexOAuth {
		m.pickerTarget = m.form.focusedKey()
		m.screen = screenModelPick
		m.loading = false
		m.modelList = codexStaticModelList()
		m.modelsErr = nil
		m.modelCursor = 0
		m.modelFilter = ""
		return m, nil
	}
	// Other providers hit their models_endpoint; custom vendors without one fall
	// through to manual text entry.
	if msg.String() == "enter" && isModelSlotKey(m.form.focusedKey()) &&
		m.form.value("models_endpoint") != "" {
		m.pickerTarget = m.form.focusedKey()
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
	case "esc", "left":
		m.screen = screenForm
		return m, textinput.Blink
	case "enter":
		if len(filtered) > 0 {
			target := m.pickerTarget
			if target == "" {
				target = "default_model"
			}
			m.form.setValue(target, filtered[m.modelCursor].ID)
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
// set. esc returns to the EDIT form. The modal trap precedes the input
// delegation: a delete/replace confirm freezes the input behind it.
func (m Model) updateKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		return m.updateConfirm(msg)
	}
	if m.keyEditing {
		return m.updateKeyInput(msg)
	}
	addRow := len(m.keys) // index of the synthetic "+ Add key…" row
	switch msg.String() {
	case "esc", "left":
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
			idx := m.keyCursor
			prompt := "Delete key " + m.keyLabel(idx) + "?"
			danger := false
			if m.keys[idx].Enabled && countEnabled(m.keys) == 1 {
				prompt += " It is the last enabled key — " + m.keyVendor + " will have no usable key."
				danger = true
			}
			// Built inline: the captured key value in arg is the delete's identity check
			// (labels are optional and non-unique); it is never rendered.
			m.confirm = &confirmModal{
				kind:   confirmDeleteKey,
				id:     strconv.Itoa(idx),
				arg:    m.keys[idx].Key,
				prompt: prompt,
				danger: danger,
			}
			return m, nil
		}
	}
	return m, nil
}

// countEnabled returns how many keys in the set are enabled.
func countEnabled(ks []secrets.KeyEntry) int {
	n := 0
	for _, e := range ks {
		if e.Enabled {
			n++
		}
	}
	return n
}

// updateKeyInput handles the add/edit password input. enter commits a non-empty
// value: an add appends and saves directly; an edit first confirms through the
// replace modal (the current value is unrecoverable) — the input stays alive and
// frozen behind it, so Cancel drops back into it. esc cancels back to the list
// without changes. The typed value is never rendered in plaintext.
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
			m.keyEditing = false
			m.keyErr = ""
			return m, m.saveKeysetCmd()
		}
		if m.keyEditIdx >= len(m.keys) {
			// The keyset refreshed under the edit and the target row is gone.
			m.keyEditing = false
			m.keyErr = "key list changed; nothing replaced"
			return m, nil
		}
		m.confirm = &confirmModal{
			kind:   confirmReplaceKey,
			id:     strconv.Itoa(m.keyEditIdx),
			arg:    m.keys[m.keyEditIdx].Key,
			prompt: "Replace key " + m.keyLabel(m.keyEditIdx) + "? The current value cannot be recovered.",
		}
		return m, nil
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

// submitAdd dispatches the add form by provider class. Required-field gaps are
// surfaced inline (no command) so the user can fix them in place; vendor-side
// errors (bad key, unreachable) come back via opDoneMsg.
func (m Model) submitAdd() (tea.Model, tea.Cmd) {
	switch m.addProtocol {
	case config.ProtocolCodexOAuth:
		return m.submitAddCodex()
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses:
		return m.submitAddOpenAI()
	default:
		return m.submitAddAnthropic()
	}
}

// modelConfigFromForm reads the shared model-roster rows back: each model slot's
// bare id recombined with its 1M toggle, effort/permission mapped off→"". Used by
// every add submitter and submitEdit so the read-back can't drift.
func (m Model) modelConfigFromForm() (defModel, strong, fast, effort, perm string) {
	return combine1M(m.form.value("default_model"), m.form.boolValue("default_1m")),
		combine1M(m.form.value("strong_model"), m.form.boolValue("strong_1m")),
		combine1M(m.form.value("fast_model"), m.form.boolValue("fast_1m")),
		offToEmpty(m.form.choiceValue("effort")),
		offToEmpty(m.form.choiceValue("permission"))
}

// submitAddOpenAI commits an OpenAI-protocol provider: the loopback base_url is
// assigned from a free daemon port; the real upstream + key ride the form.
func (m Model) submitAddOpenAI() (tea.Model, tea.Cmd) {
	name := m.form.value("name")
	upstream := m.form.value("upstream_url")
	modelsEndpoint := m.form.value("models_endpoint")
	apiKey := m.form.value("api_key")
	defaultModel, strong, fast, effort, perm := m.modelConfigFromForm()

	if missing := missingLabels(map[string]string{
		"Name":            name,
		"Upstream URL":    upstream,
		"Models endpoint": modelsEndpoint,
		"API key":         apiKey,
		"Default model":   defaultModel,
	}, []string{"Name", "Upstream URL", "Models endpoint", "API key", "Default model"}); len(missing) > 0 {
		m.form.err = "required: " + strings.Join(missing, ", ")
		return m, nil
	}
	port, err := codexproxy.ChoosePort(0)
	if err != nil {
		m.form.err = err.Error()
		return m, nil
	}
	m.form.err = ""
	m.loading = true
	return m, addVendorCmd(userops.AddRequest{
		Name:           name,
		BaseURL:        fmt.Sprintf("http://127.0.0.1:%d/", port),
		ModelsEndpoint: modelsEndpoint,
		DefaultModel:   defaultModel,
		StrongModel:    strong,
		FastModel:      fast,
		Effort:         effort,
		DefaultPerm:    perm,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		Protocol:       m.addProtocol,
		UpstreamURL:    upstream,
		APIKey:         apiKey,
		Enabled:        true,
	})
}

// submitAddCodex commits a codex provider: a free loopback port + the codex-oauth
// backend; the upstream is the fixed ChatGPT backend, so there is no key.
func (m Model) submitAddCodex() (tea.Model, tea.Cmd) {
	name := m.form.value("name")
	defaultModel, strong, fast, effort, perm := m.modelConfigFromForm()
	if missing := missingLabels(map[string]string{
		"Name":          name,
		"Default model": defaultModel,
	}, []string{"Name", "Default model"}); len(missing) > 0 {
		m.form.err = "required: " + strings.Join(missing, ", ")
		return m, nil
	}
	// Validate the name up front: a codex add may start a device login before
	// userops.Add runs, so reject a bad name here rather than authorize then fail.
	if err := ids.ValidateVendorName(name); err != nil {
		m.form.err = err.Error()
		return m, nil
	}
	// The sentinel name would resolve to the default credential a first codex already
	// holds; reject it here so a second codex never logs in then fails uniqueness.
	if m.codexDefaultTaken() && codexproxy.IsDefaultCredentialRef(name) {
		m.form.err = "name " + name + " is reserved for the default codex credential"
		return m, nil
	}
	port, err := codexproxy.ChoosePort(0)
	if err != nil {
		m.form.err = err.Error()
		return m, nil
	}
	// First codex → the default (cli-ride-capable) credential; later ones → their own,
	// keyed on the provider name. secret_ref carries this so spawn/login resolve the
	// same credential file.
	ref := codexproxy.SecretRef
	if m.codexDefaultTaken() {
		ref = name
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/", port)
	req := userops.AddRequest{
		Name:           name,
		BaseURL:        base,
		ModelsEndpoint: base + "v1/models",
		DefaultModel:   defaultModel,
		StrongModel:    strong,
		FastModel:      fast,
		Effort:         effort,
		DefaultPerm:    perm,
		SecretBackend:  codexproxy.SecretBackend,
		SecretRef:      ref,
		Protocol:       config.ProtocolCodexOAuth,
		Enabled:        true,
	}
	m.form.err = ""
	m.codexAddRef = ref
	m.codexAuthErr = ""
	switch st := codexproxy.StatusReport(ref); st.Active {
	case "own":
		// cc-fleet already has its own login for this credential — reuse, no prompt.
		m.loading = true
		return m, addVendorCmd(req)
	case "cli-ride":
		// A codex subscription is signed in on the system; let the user reuse it or log
		// in separately rather than silently riding ~/.codex. The choice is made before
		// the row is committed; only the separate-login choice then writes a login.
		m.codexPendingAdd = &req
		m.codexAuthAccount = st.Account
		m.screen = screenCodexAuth
		m.codexAuthStage = codexAuthChooseSource
		m.codexSourceCursor = 0
		return m, nil
	default: // "none" — no source; commit the row, then log in
		// Commit the provider row (vendors lock) BEFORE the login writes its token (token
		// lock), so a concurrent remove's referenced-check always sees the committed row
		// and never unlinks the fresh login. opDoneMsg routes to the login once the row
		// is in; abandoning the login rolls the row back. The committing stage owns the
		// screen meanwhile, so the form can't be escaped or repopulated mid-commit — the
		// login modal's underlay stays the form that was submitted.
		m.codexCommittedName = req.Name
		m.screen = screenCodexAuth
		m.codexAuthStage = codexAuthCommitting
		m.loading = true
		return m, addVendorCmd(req)
	}
}

// submitAddAnthropic validates the Anthropic-native add form and dispatches Add.
func (m Model) submitAddAnthropic() (tea.Model, tea.Cmd) {
	name := m.form.value("name")
	baseURL := m.form.value("base_url")
	modelsEndpoint := m.form.value("models_endpoint")
	apiKey := m.form.value("api_key")
	defaultModel, strong, fast, effort, perm := m.modelConfigFromForm()

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
		StrongModel:    strong,
		FastModel:      fast,
		Effort:         effort,
		DefaultPerm:    perm,
		SecretBackend:  "file",
		SecretRef:      name + ".key",
		APIKey:         apiKey,
		Enabled:        true,
	})
}

// submitEdit validates the edit form and dispatches userops.Edit.
func (m Model) submitEdit() (tea.Model, tea.Cmd) {
	// Recombine each model slot's bare id with its 1M toggle; the choice fields
	// map "off" → unset. The TUI is the full editor, so every field is sent
	// (including a clear) — editLocked diffs against the stored value.
	defaultModel, strongModel, fastModel, effort, defaultPerm := m.modelConfigFromForm()
	enabled := m.form.boolValue("enabled")
	req := userops.EditRequest{
		Name:         m.editName,
		DefaultModel: &defaultModel,
		StrongModel:  &strongModel,
		FastModel:    &fastModel,
		Effort:       &effort,
		DefaultPerm:  &defaultPerm,
		Enabled:      &enabled,
	}

	// The editable endpoint follows the form's class (newEditForm): openai-* edits
	// upstream_url, codex edits neither (loopback/internal), others edit base_url.
	required := map[string]string{"Default model": defaultModel}
	order := []string{"Default model"}
	switch m.addProtocol { // set to the edited vendor's protocol on edit entry
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses:
		upstream := m.form.value("upstream_url")
		models := m.form.value("models_endpoint")
		req.UpstreamURL, req.ModelsEndpoint = &upstream, &models
		required["Upstream URL"], required["Models endpoint"] = upstream, models
		order = append([]string{"Upstream URL", "Models endpoint"}, order...)
	case config.ProtocolCodexOAuth:
		// only default_model + enabled are editable
	default:
		baseURL := m.form.value("base_url")
		models := m.form.value("models_endpoint")
		req.BaseURL, req.ModelsEndpoint = &baseURL, &models
		required["Base URL"], required["Models endpoint"] = baseURL, models
		order = append([]string{"Base URL", "Models endpoint"}, order...)
	}

	if missing := missingLabels(required, order); len(missing) > 0 {
		m.form.err = "required: " + strings.Join(missing, ", ")
		return m, nil
	}
	m.form.err = ""
	m.loading = true
	return m, editVendorCmd(req)
}

// combine1M folds a model slot's bare id and its 1M toggle into the stored id: a
// blank slot stays blank (it follows the default), an "on" slot carries the [1m]
// marker, an "off" slot is stripped of it.
func combine1M(id string, on bool) string {
	if id == "" {
		return ""
	}
	if on {
		return config.With1M(id)
	}
	return config.Strip1M(id)
}

// offToEmpty maps the "off" dropdown option back to an unset ("") config value.
func offToEmpty(s string) string {
	if s == "off" {
		return ""
	}
	return s
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
