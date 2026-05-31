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

	// Active add/edit form.
	form     form
	formMode formMode
	editName string

	// Model picker: models fetched from the vendor's models_endpoint to fill the
	// default_model field. While loading, modelList is nil and modelsErr is nil;
	// the picker view branches on those.
	modelList   []models.Model
	modelCursor int
	modelsErr   error

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
	case "tab", "esc":
		return m.toList()
	case "q":
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
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
// fallback when the vendor list is unavailable.
func (m Model) updateModelPick(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.screen = screenForm
		return m, textinput.Blink
	case "up", "k":
		if m.modelCursor > 0 {
			m.modelCursor--
		}
	case "down", "j":
		if m.modelCursor < len(m.modelList)-1 {
			m.modelCursor++
		}
	case "enter":
		if len(m.modelList) > 0 {
			m.form.setValue("default_model", m.modelList[m.modelCursor].ID)
		}
		m.screen = screenForm
		return m, textinput.Blink
	}
	return m, nil
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
