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
	"github.com/ethanhq/cc-fleet/internal/teamhist"
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
	screenPickTemplate
	screenForm
	screenModelPick
	screenRemoveConfirm
	screenResult
	screenKeys      // EDIT form → "Manage API keys →": per-vendor multi-key manager
	screenSetupTmux // first-run tmux setup nudge; shown before agent-teams/hub
	screenSetup     // first-run agent-teams setup nudge; shown before the hub
)

// formMode records whether the active form is an add or an edit so submit
// knows which userops call to make.
type formMode int

const (
	modeAdd formMode = iota
	modeEdit
)

// asMode is the active level of the Agent-status master-detail board — all WITHIN
// screenSpawn (the wfMode pattern, so the refresh/tick msg ownership stays on one screen).
// →/enter descend a level; ← ascends but CLAMPS at the board's top level; esc ascends too
// and only leaves for Vendors at the top — so the entity-level detail card always RETURNS
// on esc, never exits the board. Single-choice levels collapse: one project skips L0, one
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

	// Vendor data, loaded for the Vendors list (the hub) and reused to seed the
	// edit form. vendorCursor ranges over [0, len(vendors)]; the final index is
	// the trailing "+ Add vendor…" row.
	vendors      []userops.VendorView
	vendorsErr   error
	vendorCursor int

	// Add-wizard template picker.
	tmplCursor int

	// Agent-status board (screenSpawn): live teammates + async subagent jobs in a
	// master-detail mirror of the Workflows board. asMode re-roots the levels (projects
	// → sessions → the session's Agent Teams + Subagents boxes → entity detail) WITHIN
	// this one screen; focusedProject/focusedSessionID root the deeper levels. boardEpoch
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
	// teamHistDeleteArm holds the ended team armed for record deletion: the first
	// `d` arms it (status prompt), a second `d` on the same team confirms (mirrors
	// wfDeleteArm). Any other key disarms.
	teamHistDeleteArm string
	boardEpoch        int
	asMode            asMode
	focusedProject    string
	focusedSessionID  string
	asProjectCursor   int       // L0 rail row
	asSessionCursor   int       // L1 rail row
	asBoxCursor       int       // L2 continuum row: the session's teams, then its jobs
	asEntitySrc       asRailRef // the collection L3 lists (set on descend from L2)
	asEntityCursor    int       // L3 entity row
	asCardScroll      int       // L3 detail-card scroll offset (j/k); reset on entity change
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
	// boardStatus is a one-line outcome of the last inline hide/show (so a failed
	// h/s surfaces its reason instead of relying on the next silent refresh);
	// boardStatusErr styles it as an error vs an ok confirmation. boardJobsErr is the
	// last refresh's jobs-scan failure, rendered on its OWN line — it never overwrites
	// boardStatus, which keeps its survive-the-refresh semantics.
	boardStatus    string
	boardStatusErr bool
	boardJobsErr   error
	// wfLiveOn marks the 500ms workflow-only refresh chain as scheduled, so refreshes that
	// keep seeing running leaves don't stack a second chain.
	wfLiveOn bool

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
	focusedRunID      string
	wfPhaseCursor     int
	wfAgentCursor     int
	wfActivity        map[string]activitySnapshot
	workflowStatus    string
	workflowStatusErr bool
	// wfDeleteArm holds the run id armed for deletion: the first `d` arms it (status prompt), a second
	// `d` on the same run confirms; any other key disarms (guards against an accidental single keypress).
	wfDeleteArm string
	// wfRestarting is the per-run in-flight guard: a run id is added when a stop/restart/delete is
	// dispatched and removed when its workflowCtlMsg lands, so a second x/r/d on the same run is a
	// no-op until the first completes (and a restart shows a transient "restarting …" status meanwhile).
	wfRestarting map[string]bool
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
	epoch     int
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
		return boardMsg{
			teammates:   items,
			teamErr:     err,
			jobs:        jobs,
			jobsErr:     jobsErr,
			runs:        runs,
			activity:    activity,
			sessionMeta: meta,
			endedSeen:   endedSeen,
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

// asSession is one Claude session's slice of the Agent-status board: its teams (each with
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

// groupProjects buckets the (already newest-first) sessions by their recorded working
// directory. First-seen project order therefore follows each project's most recently
// active session; the unknown bucket sorts last.
func groupProjects(sessions []asSession, meta map[string]sessiontitle.Meta) []asProject {
	order := []string{}
	byDir := map[string]*asProject{}
	for _, s := range sessions {
		dir := meta[s.sessionID].Cwd
		if dir == "" && len(s.runs) > 0 {
			dir = s.runs[0].cwd // a runs-only session still has the manifest's launch cwd
		}
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

// stopRunCmd reaps + stops the focused run and reports the outcome on the
// board status line. It is the board's only run-state mutation besides restart. The
// epoch stamps the originating Workflows visit so a result landing after the user
// left + re-entered (epoch++) is dropped (mirror workflowsMsg's gate).
func stopRunCmd(runID string, epoch int) tea.Cmd {
	return func() tea.Msg {
		err := subagent.WithRunLock(runID, func() error {
			_, e := subagent.StopRun(runID)
			return e
		})
		return workflowCtlMsg{verb: "stop", runID: runID, err: err, epoch: epoch}
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

// workflowCtlMsg carries the outcome of an x/r/d/s control on the Workflows board. Its
// handler records the status line and reloads (mirror paneVisMsg). Owned by
// screenWorkflows; epoch is the originating visit so a stale result is dropped.
type workflowCtlMsg struct {
	verb  string // "stop" | "restart" | "delete" | "save"
	runID string
	err   error
	epoch int
}

func (workflowCtlMsg) owningScreen() screen { return screenSpawn }

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

func (wfDetailMsg) owningScreen() screen { return screenSpawn }

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
		// Capture the L2 cursor row's TYPED identity before the data changes, so the
		// cursor can re-find it (a team by name, a job by id) after the refresh.
		prevBox, hadBox := m.boxRowRef()
		m.loading = false
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
			// The boxes level shows a job's card inline whenever the cursor sits on a
			// job row — keep it fresh on the same terms as the entity-level card.
			if j, ok := m.boxJob(); ok && m.jobCardStale(j) {
				m.asDetailNonce++
				m.asDetailTerminal = isTerminalLeaf(j.Status)
				detail = loadJobIOCmd(j.JobID, m.asDetailNonce, m.boardEpoch)
			}
		}
		return m, tea.Batch(detail, m.startWfLive())

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
		return m, nil

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
		delete(m.wfRestarting, msg.runID) // the op completed — clear the in-flight guard
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
		return m, loadWfLight(m.boardEpoch)

	case teamHistCtlMsg:
		// A stale result from a prior board visit must not mutate a fresh one.
		if msg.epoch != m.boardEpoch {
			return m, nil
		}
		// Surface the delete outcome on the board status line (it survives the reload),
		// then reload so the now-gone team drops off. The team name is config-sourced,
		// so scrub it like every other board surface before display.
		team := sessiontitle.CleanTitle(displayTeam(msg.team))
		if msg.err != nil {
			m.boardStatusErr = true
			m.boardStatus = fmt.Sprintf("delete %s failed: %s", team, sessiontitle.CleanTitle(msg.err.Error()))
		} else {
			m.boardStatusErr = false
			m.boardStatus = fmt.Sprintf("delete %s: ok", team)
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
		m.boardStatus = "" // clear any stale hide/show line from a prior visit
		m.boardJobsErr = nil
		m.asDetailJobID, m.asDetailPrompt, m.asDetailAnswer, m.asDetailIO = "", "", "", false
		m.asDetailSnap, m.asPromptExpanded = activitySnapshot{}, false
		m.asMateKey, m.asMateSnap, m.asMateFound = "", teammateSnapshot{}, false
		m.wfLiveOn = false
		m.asProjectCursor, m.asSessionCursor, m.asBoxCursor, m.asEntityCursor, m.asCardScroll = 0, 0, 0, 0, 0
		// Park unfocused so the FIRST accepted refresh routes by the freshly loaded
		// project/session counts — never by the previous visit's cached data.
		m.focusedProject, m.focusedSessionID, m.asMode = asNoFocus, asNoFocus, asModeBoxes
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

// updateSpawn drives the Agent-status master-detail board, branching on asMode (projects,
// sessions, the session's boxes, or an entity's inline detail). r/R reloads; tab advances
// the 3-way cycle to Dynamic Workflows; q quits. The per-mode handlers own ↑/↓, →/⏎
// (descend), ←/esc (ascend — esc additionally leaves for Vendors at the board's top level),
// and h/s (entity mode, teammate rows). The auto-refresh tick chain runs independently (see
// boardTickMsg).
func (m Model) updateSpawn(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.wfSaving {
		return m.updateWfSaveInput(msg)
	}
	if msg.String() != "d" && m.wfDeleteArm != "" {
		m.wfDeleteArm = "" // any non-d key disarms a pending run delete
		if m.workflowStatus == deleteArmPrompt {
			m.workflowStatus = ""
		}
	}
	if msg.String() != "d" && m.teamHistDeleteArm != "" {
		m.teamHistDeleteArm = "" // any non-d key disarms a pending ended-team delete
		if m.boardStatus == teamHistDeleteArmPrompt {
			m.boardStatus = ""
		}
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
	case "esc":
		return m.asAscend(true)
	case "left":
		return m.asAscend(false)
	}
	return m, nil
}

// updateAsBoxes (L2): a single ↑/↓ cursor walks the continuum — the session's team rows
// (the Agent Teams box rail), then its job rows (the Subagents box). A job row shows its
// card INLINE in the Subagents box, so it gets the card keys right here (j/k scroll, ⏎
// fold) and never needs a descend; →/⏎ descend a TEAM row into its member view. ←/esc
// ascend.
func (m Model) updateAsBoxes(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s, ok := m.focusedSession()
	n := 0
	if ok {
		n = len(s.runs) + len(s.teams) + len(s.jobs)
	}
	switch msg.String() {
	case "up":
		m.asBoxCursor = clampIndex(m.asBoxCursor-1, n)
		return m.focusBoxJobIO()
	case "down":
		m.asBoxCursor = clampIndex(m.asBoxCursor+1, n)
		return m.focusBoxJobIO()
	case "d":
		// A run row arms the run delete; an ENDED team row arms its record delete;
		// a live team row and job rows ignore d.
		if g, onRun := m.boxRun(); onRun {
			return m.armOrDelete(g.runID)
		}
		if name, onTeam := m.boxTeam(); onTeam && m.isEndedTeam(name) {
			return m.armOrDeleteTeamHist(name)
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

// focusBoxJobIO loads the job under the L2 continuum cursor into the inline card (reset
// scroll/fold, nonce-gated — focusJobIO's boxes-level sibling); a team row clears nothing
// and loads nothing.
func (m Model) focusBoxJobIO() (tea.Model, tea.Cmd) {
	j, ok := m.boxJob()
	if !ok {
		return m, nil
	}
	m.asCardScroll = 0
	m.asPromptExpanded = false
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
	if len(s.jobs) == 0 && len(s.teams) == 1 {
		return asRailRef{team: s.teams[0].name}, true
	}
	return asRailRef{}, false
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
				m.focusedProject = m.sessionMeta[s.sessionID].Cwd
				m.clampRunCursors()
				m.clampAsCursors()
				return
			}
		}
		if s, ok := m.focusedSession(); ok {
			m.asMode = asModeBoxes
			m.focusedRunID = ""
			m.focusedProject = m.sessionMeta[s.sessionID].Cwd
			m.clampAsCursors()
			return
		}
	case asModeEntity:
		if s, ok := m.focusedSession(); ok {
			// The session's recorded cwd can resolve late; keep the project link true.
			m.focusedProject = m.sessionMeta[s.sessionID].Cwd
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
			m.focusedProject = m.sessionMeta[s.sessionID].Cwd
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
	m.asBoxCursor = clampIndex(m.asBoxCursor, len(s.teams)+len(s.jobs))
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
	s, ok := m.focusedSession()
	if !ok {
		return "", false
	}
	if i := m.asBoxCursor - len(s.runs); i >= 0 && i < len(s.teams) {
		return s.teams[i].name, true
	}
	return "", false
}

// isEndedTeam reports whether a team is rendered from a history record (gone from
// live discovery) — endedSeen carries exactly those teams.
func (m Model) isEndedTeam(name string) bool {
	_, ok := m.endedSeen[name]
	return ok
}

// boxJob returns the job under the L2 continuum cursor (ok=false off the jobs range) — the
// job whose inline card the Subagents box shows.
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
		if m.wfBusy(runID) {
			return m, nil // a stop/restart/delete is already in flight — don't dispatch a second
		}
		m = m.markBusy(runID)
		return m, deleteRunCmd(runID, m.boardEpoch)
	}
	m.wfDeleteArm = runID
	m.workflowStatusErr = false
	m.workflowStatus = deleteArmPrompt
	return m, nil
}

// teamHistDeleteArmPrompt is the status shown after the first `d` on an ended team row.
const teamHistDeleteArmPrompt = "press d again to delete this ended team · any other key cancels"

// armOrDeleteTeamHist is the two-press guard for deleting an ended team's history
// record: the first `d` arms it (status prompt on the board line), a second `d` on
// the SAME team confirms and dispatches the delete (mirror armOrDelete). The
// outcome rides boardStatus, whose survive-the-refresh semantics let the follow-up
// reload re-render without the now-gone team.
func (m Model) armOrDeleteTeamHist(team string) (tea.Model, tea.Cmd) {
	if m.teamHistDeleteArm == team {
		m.teamHistDeleteArm = ""
		if m.boardStatus == teamHistDeleteArmPrompt {
			m.boardStatus = ""
		}
		return m, deleteTeamHistCmd(team, m.boardEpoch)
	}
	m.teamHistDeleteArm = team
	m.boardStatusErr = false
	m.boardStatus = teamHistDeleteArmPrompt
	return m, nil
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
// reporting the outcome on the board status line (mirror deleteRunCmd).
func deleteTeamHistCmd(team string, epoch int) tea.Cmd {
	return func() tea.Msg {
		return teamHistCtlMsg{team: team, err: teamhist.Delete(team), epoch: epoch}
	}
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
	case "esc", "left":
		return m.wfAscend()
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

// restartFocusedLeaf restarts the focused agent — workflow.Restart drops its journal entry and resumes,
// re-running it (+ any downstream leaf whose input shifted). On a still-running run the engine is stopped
// first, so every un-journaled in-flight sibling re-runs as well; the single-leaf scope is exact only once
// the run is terminal. A leaf with no persisted JournalKey falls back to a whole-run restart so r is never
// a silent no-op.
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

// armRestarting marks runID in-flight AND shows a transient "restarting <masked>…" status; the
// workflowCtlMsg handler clears the guard, and on success the status, when the restart completes.
func (m Model) armRestarting(runID string) Model {
	m = m.markBusy(runID)
	m.workflowStatusErr = false
	m.workflowStatus = "restarting " + shortRunID(sessiontitle.CleanTitle(runID)) + "…"
	return m
}

func (m Model) restartFocusedLeaf() (tea.Model, tea.Cmd) {
	job, ok := m.selectedLeaf()
	if !ok {
		return m, nil
	}
	runID, rok := m.selectedRunID()
	if !rok {
		return m, nil
	}
	if m.wfBusy(runID) {
		return m, nil
	}
	m = m.armRestarting(runID)
	return m, restartCmd(runID, job.JournalKey, m.boardEpoch)
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
		if m.wfBusy(runID) {
			return m, nil
		}
		m = m.markBusy(runID) // stop surfaces its own "stop: ok" outcome
		return m, stopRunCmd(runID, m.boardEpoch)
	case "r":
		if m.wfBusy(runID) {
			return m, nil
		}
		m = m.armRestarting(runID)
		return m, restartCmd(runID, "", m.boardEpoch) // whole run (no single leaf focused)
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
		return m, saveWorkflowCmd(runID, name, g.sessionID, g.description, m.boardEpoch)
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
