package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/panevis"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/sessiontitle"
	"github.com/ethanhq/cc-fleet/internal/subagent"
	"github.com/ethanhq/cc-fleet/internal/teamhist"
	"github.com/ethanhq/cc-fleet/internal/teardown"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// keyMsg builds a tea.KeyMsg for a key name. Single special keys map to their
// KeyType; anything else is treated as typed runes.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// step applies a message and returns the concrete Model + cmd.
func step(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return nm, cmd
}

// press applies a named key.
func press(t *testing.T, m Model, key string) (Model, tea.Cmd) {
	t.Helper()
	return step(t, m, keyMsg(key))
}

// withVendors returns a fresh model on the Vendors list with vs already loaded.
func withVendors(t *testing.T, vs ...userops.VendorView) Model {
	t.Helper()
	m, _ := step(t, NewModel(), vendorsMsg{vendors: vs})
	return m
}

func TestNewModelStartsOnVendorList(t *testing.T) {
	// Make agent-teams look configured so NewModel takes the normal hub path
	// (deterministic regardless of the ambient env). The setup-gating branch is
	// covered separately by TestNewModel_SetupGating.
	t.Setenv("CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", "1")
	m := NewModel()
	if m.screen != screenList {
		t.Fatalf("screen = %d, want screenList", m.screen)
	}
	if !m.loading {
		t.Fatal("NewModel should start loading (Init kicks off loadVendors)")
	}
	if m.Init() == nil {
		t.Fatal("Init should return the loadVendors command")
	}
}

func TestVendorListLoadsAndRenders(t *testing.T) {
	m := withVendors(t,
		userops.VendorView{Name: "deepseek", DefaultModel: "deepseek-v4-flash", Enabled: true},
		userops.VendorView{Name: "kimi", DefaultModel: "kimi-latest", Enabled: false},
	)
	if m.loading {
		t.Fatal("loading should be false after vendorsMsg")
	}
	if len(m.vendors) != 2 {
		t.Fatalf("vendors len = %d, want 2", len(m.vendors))
	}
	if out := m.View(); out == "" {
		t.Fatal("vendor list rendered empty")
	}
}

// TestVendorListCursorClamps: the cursor walks [0, len(vendors)] — the last
// index being the trailing "+ Add vendor…" row — and clamps at both ends.
func TestVendorListCursorClamps(t *testing.T) {
	m := withVendors(t,
		userops.VendorView{Name: "deepseek"}, userops.VendorView{Name: "glm"},
	)
	m, _ = press(t, m, "up")
	if m.vendorCursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0", m.vendorCursor)
	}
	for i := 0; i < 6; i++ {
		m, _ = press(t, m, "down")
	}
	if m.vendorCursor != 2 {
		t.Fatalf("after many downs: cursor = %d, want 2 (the Add row)", m.vendorCursor)
	}
}

func TestQuitKeys(t *testing.T) {
	m2, cmd := press(t, NewModel(), "q")
	if !m2.quitting || cmd == nil {
		t.Fatalf("q: quitting=%v cmd=%v, want quitting=true + quit cmd", m2.quitting, cmd)
	}
	m3, cmd := press(t, NewModel(), "ctrl+c")
	if !m3.quitting || cmd == nil {
		t.Fatalf("ctrl+c: quitting=%v cmd=%v, want quitting=true + quit cmd", m3.quitting, cmd)
	}
}

// TestTabTogglesSpawnStatus: tab from the list opens the Agents Board (and
// loads it); tab from the board returns to the Vendors list — the cycle is now
// List ↔ Spawn (the Dynamic Workflows screen folded into the Agents Board).
func TestTabTogglesSpawnStatus(t *testing.T) {
	m := withVendors(t, userops.VendorView{Name: "glm"})
	m, cmd := press(t, m, "tab")
	if m.screen != screenSpawn {
		t.Fatalf("tab: screen = %d, want screenSpawn", m.screen)
	}
	if !m.loading || cmd == nil {
		t.Fatalf("tab to board: want loading + board-load cmd, got loading=%v cmd=%v", m.loading, cmd)
	}
	if m.boardEpoch != 1 {
		t.Fatalf("entering the board should bump boardEpoch to 1, got %d", m.boardEpoch)
	}
	m, _ = step(t, m, boardMsg{
		teammates: []teardown.Teammate{
			{Name: "alice", Team: "t1", Vendor: "glm", Model: "glm-4.6", PaneID: "%3", PID: 42},
		},
		epoch: m.boardEpoch, // stamp the live epoch so the gate accepts
	})
	if m.loading || len(m.teammates) != 1 {
		t.Fatalf("after boardMsg: loading=%v teammates=%d, want false,1", m.loading, len(m.teammates))
	}
	if out := m.View(); out == "" {
		t.Fatal("board view rendered empty")
	}
	// tab from the board returns to the Vendors list (+reload).
	mback, cmd := press(t, m, "tab")
	if mback.screen != screenList || cmd == nil {
		t.Fatalf("tab from board: screen=%d cmd=%v, want screenList + reload cmd", mback.screen, cmd)
	}
	// esc from the board (at its top boxes level) also returns to the Vendors list (and reloads).
	mlist, cmd := press(t, m, "esc")
	if mlist.screen != screenList || cmd == nil {
		t.Fatalf("esc from board: screen=%d cmd=%v, want screenList + reload cmd", mlist.screen, cmd)
	}
}

// TestAddRowOpensWizard: enter on the trailing "+ Add vendor…" row (the only
// row when no vendors exist) opens the template picker; the chosen template
// prefills the form; esc returns to the Vendors list.
func TestAddRowOpensWizardAndPrefills(t *testing.T) {
	m := NewModel() // no vendors loaded -> cursor 0 == the Add row
	m, _ = press(t, m, "enter")
	if m.screen != screenPickTemplate {
		t.Fatalf("enter on Add row: screen = %d, want screenPickTemplate", m.screen)
	}
	want := Templates[0]
	m, _ = press(t, m, "enter")
	if m.screen != screenForm || m.formMode != modeAdd {
		t.Fatalf("screen=%d formMode=%d, want screenForm+modeAdd", m.screen, m.formMode)
	}
	if got := m.form.value("name"); got != want.Name {
		t.Errorf("prefill name = %q, want %q", got, want.Name)
	}
	if got := m.form.value("base_url"); got != want.BaseURL {
		t.Errorf("prefill base_url = %q, want %q", got, want.BaseURL)
	}
	if got := m.form.value("default_model"); got != want.DefaultModel {
		t.Errorf("prefill default_model = %q, want %q", got, want.DefaultModel)
	}
	if got := m.form.value("api_key"); got != "" {
		t.Errorf("api_key should start empty, got %q", got)
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("after esc: screen = %d, want screenList", m.screen)
	}
}

func TestAddFlowCustomIsBlank(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // Add row -> template picker
	for i := 0; i < len(Templates); i++ {
		m, _ = press(t, m, "down") // walk to the synthetic Custom row
	}
	if m.tmplCursor != len(Templates) {
		t.Fatalf("tmplCursor = %d, want %d (Custom)", m.tmplCursor, len(Templates))
	}
	m, _ = press(t, m, "enter")
	if m.form.value("name") != "" || m.form.value("base_url") != "" {
		t.Fatalf("custom form should be blank, got name=%q base_url=%q",
			m.form.value("name"), m.form.value("base_url"))
	}
}

func TestAddFormValidationBlocksEmptySubmit(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // Add row -> template picker
	for i := 0; i < len(Templates); i++ {
		m, _ = press(t, m, "down") // Custom -> all fields empty
	}
	m, _ = press(t, m, "enter") // blank add form

	for i := 0; i < len(m.form.fields); i++ {
		m, _ = press(t, m, "down") // walk focus onto the submit button
	}
	m, cmd := press(t, m, "enter")
	if m.screen != screenForm {
		t.Fatalf("empty submit should stay on form, screen = %d", m.screen)
	}
	if cmd != nil {
		t.Fatal("empty submit should not dispatch a command")
	}
	if m.form.err == "" {
		t.Fatal("empty submit should set a validation error")
	}
}

func TestAddFormTypingAndSubmitDispatches(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // Add row -> template picker
	m, _ = press(t, m, "enter") // choose Templates[0] -> form (prefilled)

	// Focus the api_key field (index 3) and type a key.
	m, _ = press(t, m, "down") // name -> base_url
	m, _ = press(t, m, "down") // -> models_endpoint
	m, _ = press(t, m, "down") // -> api_key
	m, _ = step(t, m, keyMsg("sk-test-123"))
	if got := m.form.value("api_key"); got != "sk-test-123" {
		t.Fatalf("typed api_key = %q, want sk-test-123", got)
	}

	m, _ = press(t, m, "down") // -> default_model
	m, _ = press(t, m, "down") // -> submit
	m, cmd := press(t, m, "enter")
	if cmd == nil {
		t.Fatal("complete submit should dispatch userops.Add command")
	}
	if !m.loading {
		t.Fatal("submit should set loading=true")
	}

	m, _ = step(t, m, opDoneMsg{verb: "add", name: Templates[0].Name})
	if m.screen != screenResult || m.resultErr {
		t.Fatalf("after success: screen=%d resultErr=%v, want screenResult+false", m.screen, m.resultErr)
	}
	// Any key returns to the Vendors list (not a menu).
	m, _ = press(t, m, "enter")
	if m.screen != screenList {
		t.Fatalf("result -> any key should return to the list, screen = %d", m.screen)
	}
}

// TestEditFromListPrefillsVendor: enter on a highlighted vendor row opens the
// edit form directly (no separate picker step).
func TestEditFromListPrefillsVendor(t *testing.T) {
	v := userops.VendorView{
		Name: "glm", BaseURL: "https://open.bigmodel.cn/api/anthropic",
		ModelsEndpoint: "https://open.bigmodel.cn/api/paas/v4/models",
		DefaultModel:   "glm-4.6", Enabled: true,
	}
	m := withVendors(t, v)
	m, _ = press(t, m, "enter") // cursor 0 == glm row -> edit form
	if m.screen != screenForm || m.formMode != modeEdit {
		t.Fatalf("screen=%d formMode=%d, want screenForm+modeEdit", m.screen, m.formMode)
	}
	if m.editName != "glm" {
		t.Errorf("editName = %q, want glm", m.editName)
	}
	if got := m.form.value("base_url"); got != v.BaseURL {
		t.Errorf("edit prefill base_url = %q, want %q", got, v.BaseURL)
	}
	if !m.form.boolValue("enabled") {
		t.Error("edit prefill enabled should be true")
	}
}

func TestEditEnabledToggle(t *testing.T) {
	f := newEditForm(userops.VendorView{Name: "x", Enabled: true})
	// Walk focus to the Enabled toggle by key (the edit form also has a trailing
	// "Manage API keys →" action row, so it is no longer the last field).
	for i := 0; i < len(f.fields) && f.focusedKey() != "enabled"; i++ {
		f, _, _ = f.Update(keyMsg("down"))
	}
	if f.focusedKey() != "enabled" {
		t.Fatalf("could not focus the enabled toggle, focusedKey = %q", f.focusedKey())
	}
	f, _, _ = f.Update(keyMsg("space"))
	if f.boolValue("enabled") {
		t.Fatal("space should toggle enabled true -> false")
	}
	f, _, _ = f.Update(keyMsg("left"))
	if !f.boolValue("enabled") {
		t.Fatal("left should toggle enabled false -> true")
	}
}

// TestDeleteFromListConfirmAndCancel: d on a highlighted vendor row opens the
// confirm; n returns to the list, y dispatches the remove.
func TestDeleteFromListConfirmAndCancel(t *testing.T) {
	mk := func() Model {
		m := withVendors(t, userops.VendorView{Name: "kimi", Enabled: true})
		m, _ = press(t, m, "d") // delete highlighted vendor -> confirm
		return m
	}

	m := mk()
	if m.screen != screenRemoveConfirm || m.removeName != "kimi" {
		t.Fatalf("screen=%d removeName=%q, want removeConfirm+kimi", m.screen, m.removeName)
	}
	m, _ = press(t, m, "n")
	if m.screen != screenList {
		t.Fatalf("n should cancel back to the list, screen = %d", m.screen)
	}

	m = mk()
	m, cmd := press(t, m, "y")
	if cmd == nil || !m.loading {
		t.Fatalf("y should dispatch remove cmd + set loading, cmd=%v loading=%v", cmd, m.loading)
	}
	m, _ = step(t, m, opDoneMsg{verb: "remove", name: "kimi", err: errors.New("boom")})
	if m.screen != screenResult || !m.resultErr {
		t.Fatalf("failed op: screen=%d resultErr=%v, want screenResult+true", m.screen, m.resultErr)
	}
}

// TestDeleteIgnoredOnAddRow: d on the trailing Add row is a no-op (nothing to
// delete).
func TestDeleteIgnoredOnAddRow(t *testing.T) {
	m := withVendors(t, userops.VendorView{Name: "glm"})
	m, _ = press(t, m, "down") // cursor -> Add row (index 1)
	if m.vendorCursor != 1 {
		t.Fatalf("cursor = %d, want 1 (Add row)", m.vendorCursor)
	}
	m, _ = press(t, m, "d")
	if m.screen != screenList {
		t.Fatalf("d on Add row should be a no-op, screen = %d", m.screen)
	}
}

func TestTemplatesSeedTable(t *testing.T) {
	if len(Templates) < 5 {
		t.Fatalf("expected >=5 seed templates, got %d", len(Templates))
	}
	byName := map[string]Template{}
	for _, tp := range Templates {
		byName[tp.Name] = tp
	}
	for _, name := range []string{"deepseek", "kimi", "glm", "qwen", "minimax"} {
		tp, ok := byName[name]
		if !ok {
			t.Errorf("missing seed template %q", name)
			continue
		}
		if tp.BaseURL == "" || tp.ModelsEndpoint == "" || tp.DefaultModel == "" {
			t.Errorf("template %q has an empty seed field: %+v", name, tp)
		}
	}
}

// addFormOnDeepseek walks Add row -> pick Templates[0] (DeepSeek) -> form. The
// resulting form has a models_endpoint prefilled, so the model picker is live.
func addFormOnDeepseek(t *testing.T) Model {
	t.Helper()
	m := NewModel()             // no vendors -> cursor 0 == Add row
	m, _ = press(t, m, "enter") // template picker (cursor 0 = DeepSeek)
	m, _ = press(t, m, "enter") // choose -> add form
	return m
}

// focusDefaultModel advances focus from the top of the add form onto the
// default_model field (name, base_url, models_endpoint, api_key, default_model).
func focusDefaultModel(t *testing.T, m Model) Model {
	t.Helper()
	for i := 0; i < 4; i++ {
		m, _ = press(t, m, "down")
	}
	if m.form.focusedKey() != "default_model" {
		t.Fatalf("focusedKey = %q, want default_model", m.form.focusedKey())
	}
	return m
}

func TestModelPickerOpensFromForm(t *testing.T) {
	m := focusDefaultModel(t, addFormOnDeepseek(t))
	m, cmd := press(t, m, "enter")
	if m.screen != screenModelPick {
		t.Fatalf("enter on default_model: screen = %d, want screenModelPick", m.screen)
	}
	if !m.loading || cmd == nil {
		t.Fatalf("want loading + a fetch cmd, got loading=%v cmd=%v", m.loading, cmd)
	}
}

func TestModelPickerOpensFromEditForm(t *testing.T) {
	m := withVendors(t, userops.VendorView{
		Name: "glm", BaseURL: "https://open.bigmodel.cn/api/anthropic",
		ModelsEndpoint: "https://open.bigmodel.cn/api/paas/v4/models",
		DefaultModel:   "glm-4.6", Enabled: true,
	})
	m, _ = press(t, m, "enter") // edit glm -> form modeEdit
	if m.screen != screenForm || m.formMode != modeEdit {
		t.Fatalf("screen=%d formMode=%d, want edit form", m.screen, m.formMode)
	}
	for i := 0; i < len(m.form.fields) && m.form.focusedKey() != "default_model"; i++ {
		m, _ = press(t, m, "down")
	}
	if m.form.focusedKey() != "default_model" {
		t.Fatalf("could not focus default_model in edit form")
	}
	m, cmd := press(t, m, "enter")
	if m.screen != screenModelPick || !m.loading || cmd == nil {
		t.Fatalf("edit-form enter on default_model: screen=%d loading=%v cmd=%v, want picker+loading+cmd",
			m.screen, m.loading, cmd)
	}
}

func TestModelPickerFillsDefaultModel(t *testing.T) {
	m := focusDefaultModel(t, addFormOnDeepseek(t))
	m, _ = press(t, m, "enter") // open picker (loading)

	m, _ = step(t, m, modelsMsg{models: []models.Model{
		{ID: "deepseek-v4-flash", OwnedBy: "deepseek"},
		{ID: "deepseek-reasoner", OwnedBy: "deepseek"},
	}})
	if m.loading {
		t.Fatal("loading should clear after modelsMsg")
	}
	if len(m.modelList) != 2 {
		t.Fatalf("modelList = %d, want 2", len(m.modelList))
	}

	m, _ = press(t, m, "down")
	if m.modelCursor != 1 {
		t.Fatalf("modelCursor = %d, want 1", m.modelCursor)
	}
	m, _ = press(t, m, "enter")
	if m.screen != screenForm {
		t.Fatalf("after pick: screen = %d, want screenForm", m.screen)
	}
	if got := m.form.value("default_model"); got != "deepseek-reasoner" {
		t.Fatalf("default_model = %q, want deepseek-reasoner (the picked id)", got)
	}
}

func TestModelPickerEmptyFallsBackToManual(t *testing.T) {
	m := addFormOnDeepseek(t)
	prefilled := m.form.value("default_model")
	m = focusDefaultModel(t, m)
	m, _ = press(t, m, "enter")
	m, _ = step(t, m, modelsMsg{models: nil})

	if out := m.View(); out == "" {
		t.Fatal("empty picker rendered an empty view")
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenForm {
		t.Fatalf("esc on empty picker: screen = %d, want screenForm", m.screen)
	}
	if got := m.form.value("default_model"); got != prefilled {
		t.Fatalf("default_model = %q, want unchanged %q", got, prefilled)
	}
}

func TestModelPickerErrorFallsBackToManual(t *testing.T) {
	m := focusDefaultModel(t, addFormOnDeepseek(t))
	m, _ = press(t, m, "enter")
	m, _ = step(t, m, modelsMsg{err: errors.New("boom-unreachable")})

	if m.modelsErr == nil {
		t.Fatal("modelsErr should be set after a failed fetch")
	}
	if out := m.View(); !strings.Contains(out, "boom-unreachable") {
		t.Fatalf("error picker view should surface the error, got %q", out)
	}
	m, _ = press(t, m, "enter")
	if m.screen != screenForm {
		t.Fatalf("enter on failed picker: screen = %d, want screenForm", m.screen)
	}
}

func TestModelPickerSkippedWhenNoEndpoint(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // Add row -> template picker
	for i := 0; i < len(Templates); i++ {
		m, _ = press(t, m, "down") // walk to the synthetic Custom row
	}
	m, _ = press(t, m, "enter") // blank add form (no models_endpoint)
	m = focusDefaultModel(t, m)

	m, _ = press(t, m, "enter")
	if m.screen != screenForm {
		t.Fatalf("enter with no endpoint opened a picker: screen = %d, want screenForm", m.screen)
	}
	if m.form.focusedKey() != "" {
		t.Fatalf("enter should have advanced to the submit button, focusedKey = %q", m.form.focusedKey())
	}
}

// pickerWith opens the model picker on the add form and loads the given models.
func pickerWith(t *testing.T, ids ...string) Model {
	t.Helper()
	m := focusDefaultModel(t, addFormOnDeepseek(t))
	m, _ = press(t, m, "enter") // open picker (loading)
	list := make([]models.Model, len(ids))
	for i, id := range ids {
		list[i] = models.Model{ID: id, OwnedBy: "deepseek"}
	}
	m, _ = step(t, m, modelsMsg{models: list})
	return m
}

func TestModelPickerFiltersByKeyword(t *testing.T) {
	m := pickerWith(t, "deepseek-chat", "deepseek-reasoner", "deepseek-coder")
	for _, r := range "reason" { // narrows to the single reasoner id
		m, _ = press(t, m, string(r))
	}
	if got := m.filteredModels(); len(got) != 1 || got[0].ID != "deepseek-reasoner" {
		t.Fatalf("filter %q → %+v, want [deepseek-reasoner]", m.modelFilter, got)
	}
	if m.modelCursor != 0 {
		t.Fatalf("modelCursor = %d, want 0 (reset on filter)", m.modelCursor)
	}
	if out := m.View(); !strings.Contains(out, "deepseek-reasoner") || strings.Contains(out, "deepseek-coder") {
		t.Fatalf("filtered view should show only the match, got %q", out)
	}
	m, _ = press(t, m, "enter")
	if got := m.form.value("default_model"); got != "deepseek-reasoner" {
		t.Fatalf("default_model = %q, want deepseek-reasoner", got)
	}
}

func TestModelPickerFilterNoMatchDoesNotPick(t *testing.T) {
	m := pickerWith(t, "deepseek-chat", "deepseek-reasoner")
	for _, r := range "zzz" {
		m, _ = press(t, m, string(r))
	}
	if got := m.filteredModels(); len(got) != 0 {
		t.Fatalf("no-match filter → %+v, want empty", got)
	}
	if out := m.View(); !strings.Contains(out, "no model matches") {
		t.Fatalf("no-match view should say so, got %q", out)
	}
	before := m.form.value("default_model")
	m, _ = press(t, m, "enter") // must not silently pick the first item
	if got := m.form.value("default_model"); got != before {
		t.Fatalf("no-match enter changed default_model %q → %q", before, got)
	}
	if m.screen != screenForm {
		t.Fatalf("enter returns to form; screen = %d, want screenForm", m.screen)
	}
}

func TestModelPickerFilterBackspaceWidens(t *testing.T) {
	m := pickerWith(t, "glm-4.5", "glm-4.5-air", "deepseek-chat")
	for _, r := range "glm" {
		m, _ = press(t, m, string(r))
	}
	if got := len(m.filteredModels()); got != 2 {
		t.Fatalf("filter glm → %d, want 2", got)
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyCtrlH}) // terminals that report Backspace as Ctrl-H
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.modelFilter != "" {
		t.Fatalf("modelFilter = %q, want empty after backspacing past the start", m.modelFilter)
	}
	if got := len(m.filteredModels()); got != 3 {
		t.Fatalf("empty filter → %d, want the full 3", got)
	}
}

func TestModelPickerFilterResetsOnReopen(t *testing.T) {
	m := pickerWith(t, "deepseek-chat")
	m, _ = press(t, m, "c")
	if m.modelFilter == "" {
		t.Fatal("filter should be set after typing")
	}
	m, _ = press(t, m, "esc")   // back to the form (focus stays on default_model)
	m, _ = press(t, m, "enter") // reopen the picker
	if m.modelFilter != "" {
		t.Fatalf("reopened picker: modelFilter = %q, want reset", m.modelFilter)
	}
}

// boardModel enters the Agents Board with the given teammates + jobs
// already loaded (screen=screenSpawn, boardEpoch=1, loading=false).
func boardModel(t *testing.T, tms []teardown.Teammate, jobs []subagent.Result) Model {
	t.Helper()
	m := withVendors(t, userops.VendorView{Name: "glm"})
	m, _ = press(t, m, "tab") // enter board (epoch 1, loading)
	// stamp the live boardEpoch so the gate accepts the refresh.
	m, _ = step(t, m, boardMsg{teammates: tms, jobs: jobs, epoch: m.boardEpoch})
	return m
}

// TestBoardSingleSessionBoxes: one session auto-focuses into the stacked Agent Teams +
// Subagents boxes: the team rail + member rows render in the first, the job rows in the
// second (a hidden row keeps its canonical health word and gains the `· hidden` suffix),
// and the job's answer text (Result.Result) never renders.
func TestBoardSingleSessionBoxes(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "t1", Vendor: "glm", Model: "glm-4.6", PaneID: "%3", PID: 42, Status: "ok", LeadSessionID: "sess-aaaaaaaa"},
			{Name: "bob", Team: "t1", Vendor: "kimi", Model: "kimi-k2", PaneID: "%4", PID: 43, Status: "error", ErrorClass: "rate_limit", Hidden: true, LeadSessionID: "sess-aaaaaaaa"},
		},
		[]subagent.Result{{
			JobID: "abcdef0123456789", Vendor: "glm", Model: "glm-4.6",
			Status: "running", StartedAt: "2026-05-26T01:02:03Z", LeadSessionID: "sess-aaaaaaaa",
			Result: "TOP-SECRET-ANSWER", // must never render
		}},
	)
	if m.asMode != asModeBoxes {
		t.Fatalf("single session should auto-focus its boxes, mode=%d", m.asMode)
	}
	out := m.View()
	for _, want := range []string{
		"sess-aaaaaaaa", // session header: the full id (the session has no title)
		"2 teammates ",  // the cursored team's header stats lead with its member count
		"Agent Teams",   // first box title
		"Subagents · 1", // second box title
		"alice", "bob",  // the previewed team's rows
		"rate_limit · hidden", // hidden row: health word + suffix
	} {
		if !strings.Contains(out, want) {
			t.Errorf("boxes view missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "TOP-SECRET-ANSWER") {
		t.Errorf("board leaked the job answer text (Result.Result):\n%s", out)
	}
}

// TestBoardMultiSessionList: >1 session (one project) parks on the session list, rail rows
// ordered by earliest activity (job-only sessions included), "(no session)" last; →/⏎
// descends into the cursored session's boxes.
func TestBoardMultiSessionList(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-bbbbbbbb", SpawnTime: 2_000_000},
			{Name: "dave", Team: "t3", PaneID: "%4"}, // no session
		},
		[]subagent.Result{
			{JobID: "job-a0000000", Vendor: "glm", Status: "done", StartedAt: "1970-01-01T00:00:01Z", LeadSessionID: "sess-aaaaaaaa"},
		},
	)
	if m.asMode != asModeSessions {
		t.Fatalf("multi session should park on the session list, mode=%d", m.asMode)
	}
	out := m.View()
	// NEWEST first: sess-b (spawn @2000s) before sess-a (job @1s); no-session last.
	posA := strings.Index(out, "sess-aaaaaaaa")
	posB := strings.Index(out, "sess-bbbbbbbb")
	posNone := strings.Index(out, "(no session)")
	if posA < 0 || posB < 0 || posNone < 0 || !(posB < posA && posA < posNone) {
		t.Fatalf("session rows missing or misordered (b=%d a=%d none=%d):\n%s", posB, posA, posNone, out)
	}
	m, _ = press(t, m, "enter")
	// sess-b is a single-team, no-jobs session (single-kind) → straight to detail.
	if m.asMode != asModeEntity || m.focusedSessionID != "sess-bbbbbbbb" {
		t.Fatalf("enter should focus the cursored session's detail view, mode=%d focus=%q", m.asMode, m.focusedSessionID)
	}
}

// TestBoardBoxContinuumAndEntityNavigation: one ↑/↓ cursor walks the teams then the job
// rows across the boxes, clamping at both ends; →/⏎ descends into entity mode where h/s
// issue commands for the selected teammate; ←/esc return to the boxes without leaving the
// board.
func TestBoardBoxContinuumAndEntityNavigation(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%3", LeadSessionID: "s"},
		{Name: "bob", Team: "t2", PaneID: "%4", LeadSessionID: "s"},
	}, []subagent.Result{{JobID: "j0000000", Status: "done", LeadSessionID: "s", StartedAt: "2026-05-26T01:00:00Z"}})
	if m.asBoxCursor != 0 {
		t.Fatalf("initial box cursor = %d, want 0", m.asBoxCursor)
	}
	m, _ = press(t, m, "up")
	if m.asBoxCursor != 0 {
		t.Fatalf("up at top: box cursor = %d, want 0 (clamped)", m.asBoxCursor)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // t1 → t2 → the job row, then clamp
	if m.asBoxCursor != 2 {
		t.Fatalf("box cursor = %d, want 2 (clamped at the job row)", m.asBoxCursor)
	}
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "enter") // descend into t1's members
	if m.asMode != asModeEntity {
		t.Fatalf("enter should descend to entity mode, mode=%d", m.asMode)
	}
	if _, cmd := press(t, m, "h"); cmd == nil {
		t.Fatal("h on a teammate row should issue a hide command")
	}
	if _, cmd := press(t, m, "s"); cmd == nil {
		t.Fatal("s on a teammate row should issue a show command")
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenSpawn || m.asMode != asModeBoxes {
		t.Fatalf("esc from entity must RETURN to the boxes, screen=%d mode=%d", m.screen, m.asMode)
	}
}

// TestBoardBoxesInlineJobCard: at the boxes level, moving the continuum cursor onto a job
// row loads and shows that job's card INLINE in the Subagents box — full sections, card
// keys (⏎ fold, j/k scroll), no descend — while a team row keeps the flat job list and the
// descend semantics.
func TestBoardBoxesInlineJobCard(t *testing.T) {
	job := subagent.Result{JobID: "j0000000", Status: "done", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z", NumTurns: 2}
	m := boardModel(t, []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}},
		[]subagent.Result{job})
	m.height = 60 // viewport tall enough that the whole inline card is on screen
	if m.asMode != asModeBoxes {
		t.Fatalf("setup: mode=%d, want boxes", m.asMode)
	}
	// The Subagents box previews the FIRST job's card even with the cursor on the team
	// row — the entry refresh already issued its io load.
	if m.asDetailNonce == 0 {
		t.Fatal("entering the boxes must issue the previewed job's io load")
	}
	m, _ = step(t, m, asDetailMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, jobID: "j0000000",
		present: true, prompt: "preview-p"})
	if out := m.View(); !strings.Contains(out, "Prompt") {
		t.Fatalf("the first job's card should preview under a team-row cursor:\n%s", out)
	}
	m, _ = press(t, m, "down") // onto the job row: same preview, now interactive
	m, _ = step(t, m, asDetailMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, jobID: "j0000000",
		present: true, prompt: "p1", answer: "THE-ANSWER",
		snap: activitySnapshot{sigs: []string{"A(1)"}}})
	out := m.View()
	for _, want := range []string{"Prompt", "THE-ANSWER", "Activity · last 3 of 1 tool calls", "Outcome"} {
		if !strings.Contains(out, want) {
			t.Errorf("the inline job card should render %q at the boxes level:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "⏎ expand") || strings.Contains(out, "→/⏎ detail") {
		t.Errorf("the footer should swap to the card keys on a job row:\n%s", out)
	}
	// ⏎ folds, never descends; → is a no-op; the refresh keeps reloading a card-visible job.
	m, _ = press(t, m, "enter")
	if m.asMode != asModeBoxes || !m.asPromptExpanded {
		t.Fatalf("⏎ on a job row must toggle the fold in place, mode=%d expanded=%v", m.asMode, m.asPromptExpanded)
	}
	m, _ = press(t, m, "right")
	if m.asMode != asModeBoxes {
		t.Fatalf("→ on a job row must stay at the boxes level, mode=%d", m.asMode)
	}
	running := job
	running.Status = "running"
	if _, cmd := step(t, m, boardMsg{teammates: []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}},
		jobs: []subagent.Result{running}, epoch: m.boardEpoch}); cmd == nil {
		t.Fatal("a refresh with the cursor on a running job row must reload the inline card")
	}
	// The team row still descends into the member view.
	m, _ = press(t, m, "up")
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity || m.asEntitySrc.jobs {
		t.Fatalf("⏎ on a team row must still descend to its members, mode=%d src=%+v", m.asMode, m.asEntitySrc)
	}
}

// TestBoardHideShowNoOpOutsideTeammateRows: h/s are no-ops on an empty board and on the
// jobs rail (no teammate to act on).
func TestBoardHideShowNoOpOutsideTeammateRows(t *testing.T) {
	m := boardModel(t, nil, nil)
	mh, cmd := press(t, m, "h")
	if cmd != nil || mh.screen != screenSpawn {
		t.Fatalf("h on an empty board: cmd=%v screen=%d, want nil + screenSpawn", cmd, mh.screen)
	}
	if _, cmd := press(t, m, "s"); cmd != nil {
		t.Fatal("s on an empty board should be a no-op (nil cmd)")
	}
	m = boardModel(t, nil, []subagent.Result{{JobID: "j0000000", Status: "done", LeadSessionID: "s", StartedAt: "2026-05-26T01:00:00Z"}})
	m, _ = press(t, m, "enter") // the job row is the only continuum row
	if m.asMode != asModeEntity {
		t.Fatalf("setup: mode=%d, want entity", m.asMode)
	}
	if _, cmd := press(t, m, "h"); cmd != nil {
		t.Fatal("h on a job row should be a no-op")
	}
	if _, cmd := press(t, m, "s"); cmd != nil {
		t.Fatal("s on a job row should be a no-op")
	}
}

// TestBoardTickReschedulesOnBoardStopsElsewhere: a current-epoch tick on the
// board reschedules; a stale-epoch tick, or any tick once off the board, stops.
func TestBoardTickReschedulesOnBoardStopsElsewhere(t *testing.T) {
	m := boardModel(t, nil, nil) // screenSpawn, epoch 1

	if _, cmd := step(t, m, boardTickMsg{epoch: m.boardEpoch}); cmd == nil {
		t.Fatal("current-epoch tick on the board should reschedule (non-nil cmd)")
	}
	if _, cmd := step(t, m, boardTickMsg{epoch: 0}); cmd != nil {
		t.Fatal("stale-epoch tick should not reschedule")
	}
	mlist, _ := press(t, m, "esc") // board → Vendors list
	if mlist.screen != screenList {
		t.Fatalf("esc should return to the list, screen = %d", mlist.screen)
	}
	if _, cmd := step(t, mlist, boardTickMsg{epoch: mlist.boardEpoch}); cmd != nil {
		t.Fatal("a tick fired while off the board should stop the chain (nil cmd)")
	}
}

// TestBoardEntityCursorClampsOnReload: when a refresh shrinks the focused group, the entity
// cursor index-clamps back into range; the group emptying entirely drops back to the rail.
func TestBoardEntityCursorClampsOnReload(t *testing.T) {
	tms := []teardown.Teammate{
		{Name: "a", Team: "t1", PaneID: "%1", LeadSessionID: "s"},
		{Name: "b", Team: "t1", PaneID: "%2", LeadSessionID: "s"},
		{Name: "c", Team: "t1", PaneID: "%3", LeadSessionID: "s"},
	}
	m := boardModel(t, tms, nil)
	m, _ = press(t, m, "enter") // entity mode on t1 (the only continuum row)
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	if m.asEntityCursor != 2 {
		t.Fatalf("entity cursor = %d, want 2", m.asEntityCursor)
	}
	m, _ = step(t, m, boardMsg{teammates: tms[:1], epoch: m.boardEpoch})
	if m.asEntityCursor != 0 || m.asMode != asModeEntity {
		t.Fatalf("after shrink: cursor=%d mode=%d, want 0 + entity", m.asEntityCursor, m.asMode)
	}
	m, _ = step(t, m, boardMsg{teammates: nil, epoch: m.boardEpoch})
	if m.asMode == asModeEntity {
		t.Fatal("an emptied group must drop entity mode")
	}
}

// TestBoardTeamsRailGrouping: interleaved input still yields one rail row per team
// (session-contiguous via groupByTeam), ordered by earliest member SpawnTime, with a
// team's members contiguous.
func TestBoardTeamsRailGrouping(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s", SpawnTime: 200},
		{Name: "bob", Team: "t2", PaneID: "%2", LeadSessionID: "s", SpawnTime: 100},
		{Name: "carol", Team: "t1", PaneID: "%3", LeadSessionID: "s", SpawnTime: 210},
	}, nil)
	s, ok := m.focusedSession()
	if !ok {
		t.Fatal("the sole session should be focused")
	}
	if len(s.teams) != 2 || s.teams[0].name != "t2" || s.teams[1].name != "t1" {
		t.Fatalf("teams = %+v, want [t2 t1] (earliest SpawnTime first)", s.teams)
	}
	if len(s.teams[1].members) != 2 {
		t.Fatalf("t1 members = %d, want 2 (alice+carol contiguous)", len(s.teams[1].members))
	}
}

// TestBoardBoxReanchorsTypedIdentity: the L2 cursor re-finds its row by typed identity
// after a refresh — a job row stays a job row even when a real team is named "jobs" and the
// indices shift.
func TestBoardBoxReanchorsTypedIdentity(t *testing.T) {
	tms := []teardown.Teammate{
		{Name: "x", Team: "jobs", PaneID: "%1", LeadSessionID: "s", SpawnTime: 100},
		{Name: "y", Team: "t2", PaneID: "%2", LeadSessionID: "s", SpawnTime: 200},
	}
	jobs := []subagent.Result{{JobID: "j0000000", Status: "done", LeadSessionID: "s", StartedAt: "2026-05-26T01:00:00Z"}}
	m := boardModel(t, tms, jobs)
	// Continuum: team "jobs"(0), t2(1), the job row(2). Park on the job row.
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	if ref, ok := m.boxRowRef(); !ok || ref.jobID == "" {
		t.Fatalf("setup: cursor should sit on the job row, ref=%+v", ref)
	}
	// Dropping t2 shifts the indices; the cursor must follow the job's identity.
	m, _ = step(t, m, boardMsg{teammates: tms[:1], jobs: jobs, epoch: m.boardEpoch})
	if ref, ok := m.boxRowRef(); !ok || ref.jobID == "" {
		t.Fatalf("after refresh: cursor lost the job row, ref=%+v", ref)
	}
}

// TestBoardSessionHeaderUsesResolvedTitle: the session header shows the resolved /rename
// title for the focused session.
func TestBoardSessionHeaderUsesResolvedTitle(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-aaaaaaaa"},
		},
		[]subagent.Result{{
			JobID: "job-a000", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T02:00:00Z",
			LeadSessionID: "sess-aaaaaaaa", Result: "TOP-SECRET",
		}},
	)
	m.sessionMeta = map[string]sessiontitle.Meta{"sess-aaaaaaaa": {Title: "Readable Session Name"}}
	out := m.View()
	if !strings.Contains(out, "Readable Session Name (sess-aaa…)") {
		t.Fatalf("the session header should show the resolved title:\n%s", out)
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("session title rendering must not leak job answers:\n%s", out)
	}
}

// TestBoardEntityDetailCard: →/⏎ opens the inline detail card showing UNtruncated
// vendor/model (the row clips them); esc returns to the rail with the screen unchanged.
func TestBoardEntityDetailCard(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "worker-1", Team: "alpha", Vendor: "xiaomimimo", Model: "mimo-v2-flash", PaneID: "%7", PID: 4242, Status: "ok", LeadSessionID: "s"},
	}, nil)
	// A single-team, no-jobs session lands straight on the detail view.
	if m.screen != screenSpawn || m.asMode != asModeEntity {
		t.Fatalf("entry: screen=%d mode=%d, want screenSpawn + entity", m.screen, m.asMode)
	}
	out := m.View()
	// "xiaomimimo" (10) and "mimo-v2-flash" (13) exceed the row's truncation widths; the
	// detail card must render them in full.
	for _, want := range []string{"worker-1", "alpha", "xiaomimimo", "mimo-v2-flash", "%7", "4242"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail card missing %q:\n%s", want, out)
		}
	}
	// The card IS the board's top level here: ← clamps (stays), esc leaves for Vendors.
	m, _ = press(t, m, "left")
	if m.screen != screenSpawn || m.asMode != asModeEntity {
		t.Fatalf("left at the top: screen=%d mode=%d, want screenSpawn + entity (clamped)", m.screen, m.asMode)
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("esc at the top should leave for Vendors, screen=%d", m.screen)
	}
}

// TestBoardEnterNoOpWhenEmpty: enter with no rows stays on the (empty) boxes level.
func TestBoardEnterNoOpWhenEmpty(t *testing.T) {
	m := boardModel(t, nil, nil)
	m, _ = press(t, m, "enter")
	if m.screen != screenSpawn || m.asMode != asModeBoxes {
		t.Fatalf("enter with no rows: screen=%d mode=%d, want screenSpawn + boxes (no-op)", m.screen, m.asMode)
	}
}

// TestBoardTickSurvivesEntityMode: the detail level lives INSIDE screenSpawn, so a
// current-epoch tick keeps rescheduling while a detail card is open (the old separate
// detail screen used to kill the chain).
func TestBoardTickSurvivesEntityMode(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}}, nil)
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity {
		t.Fatalf("setup: mode=%d, want entity", m.asMode)
	}
	if _, cmd := step(t, m, boardTickMsg{epoch: m.boardEpoch}); cmd == nil {
		t.Fatal("a current-epoch tick must keep rescheduling while the detail card is open")
	}
}

// TestBoardMultiProjectRoutesToProjects: sessions whose transcripts record different
// working directories park on the project rail; descending into a single-session project
// collapses straight to its boxes, and unresolved-cwd sessions bucket as "(no project)"
// last.
func TestBoardMultiProjectRoutesToProjects(t *testing.T) {
	tms := []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-aaaaaaaa", SpawnTime: 100},
		{Name: "bob", Team: "t2", PaneID: "%2", LeadSessionID: "sess-bbbbbbbb", SpawnTime: 200},
		{Name: "carol", Team: "t3", PaneID: "%3", LeadSessionID: "sess-cccccccc", SpawnTime: 300},
	}
	meta := map[string]sessiontitle.Meta{
		"sess-aaaaaaaa": {Cwd: "/proj/alpha"},
		"sess-bbbbbbbb": {Cwd: "/proj/beta"},
	}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{teammates: tms, sessionMeta: meta, epoch: m.boardEpoch})
	if m.asMode != asModeProjects {
		t.Fatalf("multi project should park on the project rail, mode=%d", m.asMode)
	}
	out := m.View()
	// NEWEST first: beta (spawn 200) before alpha (100); the unresolved bucket last.
	posA := strings.Index(out, "proj/alpha")
	posB := strings.Index(out, "proj/beta")
	posNone := strings.Index(out, "(no project)")
	if posA < 0 || posB < 0 || posNone < 0 || !(posB < posA && posA < posNone) {
		t.Fatalf("project rows missing or misordered (b=%d a=%d none=%d):\n%s", posB, posA, posNone, out)
	}
	// Descending into a single-session, single-team project collapses straight to the
	// detail view (both the session and the boxes levels are skipped).
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity || m.focusedSessionID != "sess-bbbbbbbb" || m.focusedProject != "/proj/beta" {
		t.Fatalf("enter on a single-session project should land on its detail view, mode=%d session=%q project=%q",
			m.asMode, m.focusedSessionID, m.focusedProject)
	}
	// Ascending skips the degenerate levels and returns to the project rail.
	m, _ = press(t, m, "left")
	if m.asMode != asModeProjects {
		t.Fatalf("left from a single-session project's detail should return to the project rail, mode=%d", m.asMode)
	}
}

// TestBoardReentryRoutesOnFreshLoad: a re-entry must route from the freshly loaded session
// count, never the previous visit's cached data — a stale single-session focus can't
// suppress the session list when the fresh load shows several sessions.
func TestBoardReentryRoutesOnFreshLoad(t *testing.T) {
	one := []teardown.Teammate{{Name: "a", Team: "t1", PaneID: "%1", LeadSessionID: "sess-aaaaaaaa"}}
	m := boardModel(t, one, nil) // single-kind session → auto-focused detail view on sess-a
	if m.asMode != asModeEntity {
		t.Fatalf("setup: mode=%d, want entity", m.asMode)
	}
	mlist, _ := press(t, m, "esc")
	mb, _ := press(t, mlist, "tab") // re-enter: the old slices are still cached
	two := append(one, teardown.Teammate{Name: "b", Team: "t2", PaneID: "%2", LeadSessionID: "sess-bbbbbbbb"})
	mb, _ = step(t, mb, boardMsg{teammates: two, epoch: mb.boardEpoch})
	if mb.asMode != asModeSessions {
		t.Fatalf("re-entry with a fresh multi-session load must park on the session list, mode=%d", mb.asMode)
	}
}

// TestJobElapsedNeverAgesTerminalJobs: a terminal job whose record carries no duration
// renders no elapsed at all — never a live, growing since-StartedAt figure.
func TestJobElapsedNeverAgesTerminalJobs(t *testing.T) {
	if got := jobElapsed(subagent.Result{Status: "done", StartedAt: "2026-05-01T00:00:00Z"}); got != "" {
		t.Fatalf("terminal durationless job elapsed = %q, want empty", got)
	}
	if got := jobElapsed(subagent.Result{Status: "running", StartedAt: "2026-05-01T00:00:00Z"}); got == "" {
		t.Fatal("a running job should show a live elapsed")
	}
	if got := jobElapsed(subagent.Result{Status: "done", DurationMs: 7000}); got != "7s" {
		t.Fatalf("terminal job with a duration = %q, want 7s", got)
	}
}

// TestBoardJobsErrOwnLine: a jobs-scan failure renders on its own line and does NOT clobber
// a surfaced hide/show outcome.
func TestBoardJobsErrOwnLine(t *testing.T) {
	tms := []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}}
	m := boardModel(t, tms, nil)
	m, _ = step(t, m, paneVisMsg{res: panevis.Result{OK: true, Action: "hide", Name: "alice"}})
	m, _ = step(t, m, boardMsg{teammates: tms, jobsErr: errors.New("jobs dir unreadable"), epoch: m.boardEpoch})
	out := m.View()
	if !strings.Contains(out, "jobs unavailable: jobs dir unreadable") {
		t.Fatalf("jobs-scan failure should surface on its own line:\n%s", out)
	}
	if !strings.Contains(out, "hide alice: ok") {
		t.Fatalf("the jobs error line must not clobber the hide/show outcome:\n%s", out)
	}
}

// TestBoardUptimeFromSpawnTime: a recorded SpawnTime renders an "up …" figure on the row;
// an unrecorded one omits it.
func TestBoardUptimeFromSpawnTime(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "timed", Team: "t1", PaneID: "%1", LeadSessionID: "s", SpawnTime: time.Now().Add(-5 * time.Minute).UnixMilli()},
		{Name: "untimed", Team: "t1", PaneID: "%2", LeadSessionID: "s"},
	}, []subagent.Result{{JobID: "j0000000", Status: "done", LeadSessionID: "s", StartedAt: "2026-05-26T01:00:00Z"}})
	out := m.View()
	if !strings.Contains(out, "· up 5m") {
		t.Fatalf("a SpawnTime-bearing row should render its uptime:\n%s", out)
	}
	if strings.Count(out, "· up ") != 1 {
		t.Fatalf("an unrecorded SpawnTime must omit the uptime:\n%s", out)
	}
}

// TestBoardHeaderShowsCursoredAgentStats: with the cursor on a single agent, the session
// header's right slot shows THAT agent's own stats — a job's tokens/elapsed/started, a
// teammate's uptime — and falls back to the session rollup on an L2 team row.
func TestBoardHeaderShowsCursoredAgentStats(t *testing.T) {
	job := subagent.Result{JobID: "j0000000", Status: "done", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z", DurationMs: 7000,
		Usage: &subagent.Usage{InputTokens: 1200, OutputTokens: 340}}
	m := boardModel(t, nil, []subagent.Result{job}) // jobs-only → straight to the job's detail view
	if out := m.View(); !strings.Contains(out, "↑ 1.2k ctx · ↓ 340 out · 7s · 05-26 01:00") {
		t.Fatalf("the header should show the cursored job's stats:\n%s", out)
	}
	mate := teardown.Teammate{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s",
		SpawnTime: time.Now().Add(-5 * time.Minute).UnixMilli()}
	m = boardModel(t, []teardown.Teammate{mate}, nil) // single team → the teammate's detail view
	if out := m.View(); !strings.Contains(out, "5m · ") {
		t.Fatalf("the header should show the cursored teammate's age:\n%s", out)
	}
	// L2: a team row carries the TEAM's stats (age + spawn; tokens once the aggregate
	// lands); moving onto the job row swaps in the job's stats.
	m = boardModel(t, []teardown.Teammate{mate}, []subagent.Result{job})
	if out := m.View(); !strings.Contains(out, "1 teammates · 5m · ") {
		t.Fatalf("an L2 team row should carry the team's stats:\n%s", out)
	}
	m, _ = step(t, m, asTeamMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, key: "t1", ctx: 92032, out: 48659})
	if out := m.View(); !strings.Contains(out, "1 teammates · ↑ 92.0k · ↓ 48.7k · 5m") {
		t.Fatalf("the landed aggregate should join the team header stats:\n%s", out)
	}
	m, _ = press(t, m, "down")
	if out := m.View(); !strings.Contains(out, "↑ 1.2k ctx") {
		t.Fatalf("an L2 job row should swap the cursored job's stats into the header:\n%s", out)
	}
}

// TestBoardCardScrollResetAndClamp: j/k scroll clamps to the card content (zero for a short
// card) and entity movement resets the offset.
func TestBoardCardScrollResetAndClamp(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "a", Team: "t1", PaneID: "%1", LeadSessionID: "s"},
		{Name: "b", Team: "t1", PaneID: "%2", LeadSessionID: "s"},
	}, nil)
	m.height = 40 // viewport taller than the card, so the clamp floor is 0
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "j")
	if m.asCardScroll != 0 {
		t.Fatalf("a short card must clamp the scroll at 0, got %d", m.asCardScroll)
	}
	m.asCardScroll = 99 // a stale offset from a longer card
	m, _ = press(t, m, "down")
	if m.asCardScroll != 0 {
		t.Fatalf("entity movement must reset the card scroll, got %d", m.asCardScroll)
	}
}

// TestBoardJobCardLeafParity: the standalone-job detail card carries the wf agent card's
// sections — collapsed Prompt (⏎ expands), the Activity last-3 feed, the Output body — fed
// by the nonce-gated io load issued when the board lands on the jobs detail view.
func TestBoardJobCardLeafParity(t *testing.T) {
	job := subagent.Result{JobID: "j0000000", Status: "done", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z", NumTurns: 2}
	m := boardModel(t, nil, []subagent.Result{job}) // jobs-only → straight to the detail view
	if m.asMode != asModeEntity || m.asDetailNonce == 0 {
		t.Fatalf("setup: mode=%d nonce=%d, want entity + an issued io load", m.asMode, m.asDetailNonce)
	}
	m, _ = step(t, m, asDetailMsg{
		nonce: m.asDetailNonce, epoch: m.boardEpoch, jobID: "j0000000", present: true,
		prompt: "line one\nline two\nline three\nline four\nline five\nline six",
		answer: "THE-ANSWER-BODY",
		snap:   activitySnapshot{sigs: []string{"A(1)", "B(2)", "C(3)", "D(4)"}},
	})
	// Assert on the full card line set — the box viewport scrolls, so sections can sit
	// below the visible fold.
	card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	for _, want := range []string{
		"Prompt · 6 lines · ⏎ expand",
		"Activity · last 3 of 4 tool calls",
		"D(4)", // the last three sigs show…
		"Output", "THE-ANSWER-BODY",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("job card missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "A(1)") {
		t.Errorf("Activity should show only the LAST 3 signatures:\n%s", card)
	}
	if strings.Contains(card, "line six") {
		t.Errorf("the folded prompt must hide its tail:\n%s", card)
	}
	m, _ = press(t, m, "enter") // expand the prompt
	card = strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	if !strings.Contains(card, "line six") {
		t.Fatalf("⏎ should expand the full prompt:\n%s", card)
	}
}

// TestBoardJobCardStaleNonceDropped: an io read answering a previously-focused job is
// dropped, never shown on the wrong card.
func TestBoardJobCardStaleNonceDropped(t *testing.T) {
	job := subagent.Result{JobID: "j0000000", Status: "done", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z"}
	m := boardModel(t, nil, []subagent.Result{job})
	stale := asDetailMsg{nonce: m.asDetailNonce - 1, epoch: m.boardEpoch, jobID: "j0000000", present: true, answer: "STALE-ANSWER"}
	m, _ = step(t, m, stale)
	if m.asDetailIO || strings.Contains(m.View(), "STALE-ANSWER") {
		t.Fatal("a stale-nonce io read must be dropped")
	}
	prior := asDetailMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch - 1, jobID: "j0000000", present: true, answer: "PRIOR-VISIT"}
	m, _ = step(t, m, prior)
	if m.asDetailIO || strings.Contains(m.View(), "PRIOR-VISIT") {
		t.Fatal("a prior-visit (stale-epoch) io read must be dropped")
	}
}

// TestBoardJobCardReloadsOnTerminalFlip: the refresh that first sees the focused job
// terminal still issues one io load (the final .answer lands), and the NEXT refresh of the
// stable terminal job issues none.
func TestBoardJobCardReloadsOnTerminalFlip(t *testing.T) {
	running := subagent.Result{JobID: "j0000000", Status: "running", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z"}
	done := running
	done.Status = "done"
	m := boardModel(t, nil, []subagent.Result{running})
	mm, cmd := step(t, m, boardMsg{jobs: []subagent.Result{done}, epoch: m.boardEpoch})
	if cmd == nil {
		t.Fatal("the running→done refresh must reload the focused job's io")
	}
	// Land the issued read (production delivers it between refreshes), then a stable
	// terminal focused job must stop reloading.
	mm, _ = step(t, mm, asDetailMsg{nonce: mm.asDetailNonce, epoch: mm.boardEpoch, jobID: "j0000000", present: true, answer: "final"})
	if _, cmd := step(t, mm, boardMsg{jobs: []subagent.Result{done}, epoch: mm.boardEpoch}); cmd != nil {
		t.Fatal("a stable terminal focused job must not reload io every refresh")
	}
}

// TestBoardJobCardNotPersistedNote: a job without io side files shows the not-persisted
// note instead of empty sections.
func TestBoardJobCardNotPersistedNote(t *testing.T) {
	job := subagent.Result{JobID: "j0000000", Status: "done", LeadSessionID: "s",
		StartedAt: "2026-05-26T01:00:00Z"}
	m := boardModel(t, nil, []subagent.Result{job})
	m, _ = step(t, m, asDetailMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, jobID: "j0000000", present: false})
	if out := m.View(); !strings.Contains(out, "not persisted") {
		t.Fatalf("an io-less job should show the not-persisted note:\n%s", out)
	}
}

// TestTeammateCardMessagesAndOutput: the focused teammate's card renders the merged
// Messages block (pending ● + consumed ○, newest first, one line each collapsed, ⏎
// expands the bodies), the Activity feed, the always-expanded Output block (newest
// first under timestamp rules), and the Overview tokens field — all fed by the
// nonce-gated asMateMsg load.
func TestTeammateCardMessagesAndOutput(t *testing.T) {
	mate := teardown.Teammate{Name: "alice", Team: "t1", Model: "glm-4.6", PaneID: "%1", PID: 7, Status: "ok", LeadSessionID: "s"}
	m := boardModel(t, []teardown.Teammate{mate}, nil) // single team, no jobs → straight to detail
	if m.asMode != asModeEntity || m.asDetailNonce == 0 {
		t.Fatalf("setup: mode=%d nonce=%d, want entity + an issued mate load", m.asMode, m.asDetailNonce)
	}
	if card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n"); !strings.Contains(card, "(loading…)") {
		t.Fatalf("the card should show the loading note until the payload lands:\n%s", card)
	}
	m, _ = step(t, m, asMateMsg{
		nonce: m.asDetailNonce, epoch: m.boardEpoch, key: mateKey(mate), found: true,
		snap: teammateSnapshot{
			activity: activitySnapshot{sigs: []string{"A(1)", "B(2)", "C(3)", "D(4)"}},
			msgs: []mateMessage{
				{from: "team-lead", summary: "older subject", body: "older body text", ts: "2026-06-07T02:11:00Z"},
				{from: "team-lead", summary: "Subj-X", body: "full assignment body", ts: "2026-06-07T02:31:00Z", pending: true},
			},
			outputs: []mateOutput{
				{text: "older output", ts: "2026-06-07T02:20:00Z"},
				{text: "newest output", ts: "2026-06-07T02:40:00Z"},
			},
			ctxTok: 92032, outTok: 48659,
		},
	})
	card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	for _, want := range []string{
		"tokens", "↑ 92.0k ctx · ↓ 48.7k out",
		"Messages · 2 · 1 pending · ⏎ expand",
		"Subj-X", "older subject",
		"06-07 02:31",
		"Activity · last 3 of 4 tool calls", "D(4)",
		"Output · 2 messages", "newest output", "older output",
		"06-07 02:40",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("teammate card missing %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "full assignment body") {
		t.Errorf("a collapsed Messages block must not render bodies:\n%s", card)
	}
	// Newest first in both blocks.
	if i, j := strings.Index(card, "Subj-X"), strings.Index(card, "older subject"); i > j {
		t.Errorf("Messages must render newest first (Subj-X@%d, older@%d):\n%s", i, j, card)
	}
	if i, j := strings.Index(card, "newest output"), strings.Index(card, "older output"); i > j {
		t.Errorf("Output must render newest first:\n%s", card)
	}
	m, _ = press(t, m, "enter") // expand the message bodies
	card = strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	if !strings.Contains(card, "full assignment body") || !strings.Contains(card, "older body text") {
		t.Fatalf("⏎ should expand the full message bodies:\n%s", card)
	}
	// The expanded card swaps the header hint away and the cursored teammate's header
	// stats include the transcript tokens.
	if strings.Contains(card, "⏎ expand") {
		t.Errorf("the expanded Messages header should drop the expand hint:\n%s", card)
	}
	if out := m.View(); !strings.Contains(out, "↑ 92.0k ctx · ↓ 48.7k out") {
		t.Errorf("the session header should carry the focused teammate's tokens:\n%s", out)
	}
}

// TestTeammateCardLoadsEveryRefresh: a focused teammate reloads its messages/transcript on
// EVERY accepted refresh (teammates are long-lived — no terminal short-circuit), and the
// entity cursor movement issues a fresh load too; a stale nonce or epoch payload is dropped.
func TestTeammateCardLoadsEveryRefresh(t *testing.T) {
	tms := []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"},
		{Name: "bob", Team: "t1", PaneID: "%2", LeadSessionID: "s"},
	}
	m := boardModel(t, tms, nil)
	if m.asMode != asModeEntity {
		t.Fatalf("setup: mode=%d, want entity", m.asMode)
	}
	for i := 0; i < 2; i++ {
		var cmd tea.Cmd
		m, cmd = step(t, m, boardMsg{teammates: tms, epoch: m.boardEpoch})
		if cmd == nil {
			t.Fatalf("refresh %d: a focused teammate must reload its detail payload", i+1)
		}
	}
	before := m.asDetailNonce
	m, cmd := press(t, m, "down")
	if cmd == nil || m.asDetailNonce != before+1 {
		t.Fatal("entity cursor movement must issue a fresh mate load")
	}
	stale := asMateMsg{nonce: m.asDetailNonce - 1, epoch: m.boardEpoch, key: "k", found: true,
		snap: teammateSnapshot{outputs: []mateOutput{{text: "STALE-OUTPUT"}}}}
	m, _ = step(t, m, stale)
	if m.asMateKey != "" || strings.Contains(m.View(), "STALE-OUTPUT") {
		t.Fatal("a stale-nonce mate payload must be dropped")
	}
	prior := asMateMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch - 1, key: "k", found: true,
		snap: teammateSnapshot{outputs: []mateOutput{{text: "PRIOR-VISIT"}}}}
	m, _ = step(t, m, prior)
	if m.asMateKey != "" || strings.Contains(m.View(), "PRIOR-VISIT") {
		t.Fatal("a prior-visit (stale-epoch) mate payload must be dropped")
	}
}

// TestTeammateCardRespawnDropsStalePayload: a respawned same-named teammate (new pid) must
// not render its predecessor's payload while the fresh load is in flight — the payload key
// carries the pid generation, so the card falls back to the loading note.
func TestTeammateCardRespawnDropsStalePayload(t *testing.T) {
	v1 := teardown.Teammate{Name: "alice", Team: "t1", PaneID: "%1", PID: 42, LeadSessionID: "s"}
	m := boardModel(t, []teardown.Teammate{v1}, nil)
	m, _ = step(t, m, asMateMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, key: mateKey(v1), found: true,
		snap: teammateSnapshot{outputs: []mateOutput{{text: "OLD-GENERATION"}}}})
	if card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n"); !strings.Contains(card, "OLD-GENERATION") {
		t.Fatalf("setup: the loaded payload should render on its own generation:\n%s", card)
	}
	v2 := v1
	v2.PID, v2.PaneID = 43, "%9"
	m, cmd := step(t, m, boardMsg{teammates: []teardown.Teammate{v2}, epoch: m.boardEpoch})
	if cmd == nil {
		t.Fatal("the respawned teammate must trigger a fresh load")
	}
	card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	if strings.Contains(card, "OLD-GENERATION") {
		t.Fatalf("the new generation's card must not render the predecessor's payload:\n%s", card)
	}
	if !strings.Contains(card, "(loading…)") {
		t.Fatalf("the new generation's card should show the loading note:\n%s", card)
	}
}

// TestTeammateCardNoTranscript: an unlocatable transcript renders the no-transcript note
// and omits the Activity/Output sections; pending messages (the merged inbox backlog)
// still show.
func TestTeammateCardNoTranscript(t *testing.T) {
	mate := teardown.Teammate{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}
	m := boardModel(t, []teardown.Teammate{mate}, nil)
	m, _ = step(t, m, asMateMsg{nonce: m.asDetailNonce, epoch: m.boardEpoch, key: mateKey(mate),
		snap: teammateSnapshot{msgs: []mateMessage{{from: "team-lead", summary: "queued task", ts: "2026-06-07T02:31:00Z", pending: true}}}})
	card := strings.Join(m.entityDetailLines(m.asEntityRightWidth()), "\n")
	for _, want := range []string{"Messages · 1 · 1 pending", "queued task", "(no transcript found)"} {
		if !strings.Contains(card, want) {
			t.Errorf("transcript-less card missing %q:\n%s", want, card)
		}
	}
	for _, absent := range []string{"Activity ·", "Output ·"} {
		if strings.Contains(card, absent) {
			t.Errorf("transcript-less card must omit %q:\n%s", absent, card)
		}
	}
}

// TestAgentBoardNewSurfacesKeySafe: canaries planted in Result.Result and ErrorMsg must not
// reach any board surface — the rows, the rail, or the entity detail card (which renders
// the canonical ErrorCode only) — and the teammate card's inbox/transcript text shows ONLY
// on the focused teammate's own card, never on another card or an upper level.
func TestAgentBoardNewSurfacesKeySafe(t *testing.T) {
	const answerCanary = "ANSWER-CANARY-sk-9f8e7d"
	const errCanary = "ERRMSG-CANARY-sk-1a2b3c"
	job := subagent.Result{
		JobID: "deadbeef00000000", Vendor: "glm", Model: "glm-4.6",
		Status: "failed", StartedAt: "2026-05-26T01:00:00Z", LeadSessionID: "s",
		Result: answerCanary, ErrorCode: "SUBAGENT_FAILED", ErrorMsg: errCanary,
		Usage: &subagent.Usage{InputTokens: 1200, OutputTokens: 340},
	}
	m := boardModel(t, nil, []subagent.Result{job})
	groups := m.View()
	mCard, _ := press(t, m, "enter")
	// Even with the io side files loaded, Result.Result and ErrorMsg never render — the
	// card's Output comes from the .answer file alone.
	mCard, _ = step(t, mCard, asDetailMsg{nonce: mCard.asDetailNonce, epoch: mCard.boardEpoch, jobID: job.JobID,
		present: true, prompt: "p", answer: "file answer"})
	// The card line set is the full surface (the box viewport scrolls), so the witness
	// checks it alongside the rendered views.
	card := strings.Join(mCard.entityDetailLines(mCard.asEntityRightWidth()), "\n")
	for name, out := range map[string]string{"groups": groups, "view": mCard.View(), "card": card} {
		if strings.Contains(out, answerCanary) || strings.Contains(out, errCanary) {
			t.Errorf("%s leaked a canary:\n%s", name, out)
		}
	}
	if !strings.Contains(card, "SUBAGENT_FAILED") {
		t.Errorf("the card should render the canonical ErrorCode:\n%s", card)
	}

	// Teammate surfaces: message text and transcript-extracted text are focused-card-only.
	const msgCanary = "MSG-CANARY-sk-4d5e6f"
	const sigCanary = "SIG-CANARY-sk-7a8b9c"
	const outCanary = "OUTPUT-CANARY-sk-0d1e2f"
	alice := teardown.Teammate{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "s"}
	tms := []teardown.Teammate{alice, {Name: "bob", Team: "t1", PaneID: "%2", LeadSessionID: "s"}}
	mm := boardModel(t, tms, []subagent.Result{{JobID: "j0000000", Status: "done", LeadSessionID: "s", StartedAt: "2026-05-26T01:00:00Z"}})
	boxes := mm.View()            // L2: the stacked boxes — no detail payload loaded yet
	mm, _ = press(t, mm, "enter") // descend onto t1's members (cursor on alice)
	mm, _ = step(t, mm, asMateMsg{
		nonce: mm.asDetailNonce, epoch: mm.boardEpoch, key: mateKey(alice), found: true,
		snap: teammateSnapshot{
			activity: activitySnapshot{sigs: []string{"Bash(" + sigCanary + ")"}},
			msgs:     []mateMessage{{from: "team-lead", summary: msgCanary, body: msgCanary, ts: "2026-06-07T02:31:00Z", pending: true}},
			outputs:  []mateOutput{{text: outCanary, ts: "2026-06-07T02:40:00Z"}},
		},
	})
	focused := strings.Join(mm.entityDetailLines(mm.asEntityRightWidth()), "\n")
	for _, want := range []string{msgCanary, sigCanary, outCanary} {
		if !strings.Contains(focused, want) {
			t.Errorf("the FOCUSED teammate card should render its own payload %q:\n%s", want, focused)
		}
	}
	mmBob, _ := press(t, mm, "down") // cursor → bob: alice's payload must vanish (key mismatch)
	mmUp, _ := press(t, mm, "esc")   // back to the boxes level
	bobCard := strings.Join(mmBob.entityDetailLines(mmBob.asEntityRightWidth()), "\n")
	for name, out := range map[string]string{"boxes": boxes, "bob-view": mmBob.View(), "bob-card": bobCard, "ascended": mmUp.View()} {
		for _, canary := range []string{msgCanary, sigCanary, outCanary} {
			if strings.Contains(out, canary) {
				t.Errorf("%s leaked teammate detail text %q:\n%s", name, canary, out)
			}
		}
	}
}

// TestBoardSurfacesHideShowResult: an inline hide/show is surfaced on a status
// line (error for a failure, ok for success) rather than failing silently,
// survives the follow-up refresh, and clears on a fresh board entry.
func TestBoardSurfacesHideShowResult(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1"}}, nil)

	// A failed hide surfaces code + reason + suggestion and triggers a reload.
	m, cmd := step(t, m, paneVisMsg{res: panevis.Result{
		Action: "hide", Name: "alice", ErrorCode: panevis.ErrTmuxFailed,
		ErrorMsg: "break-pane failed", Suggestion: "check tmux",
	}})
	if cmd == nil {
		t.Fatal("a hide outcome should still reload the board")
	}
	if !m.boardStatusErr {
		t.Fatalf("a failed hide should set an error status, got err=%v msg=%q", m.boardStatusErr, m.boardStatus)
	}
	out := m.View()
	for _, want := range []string{"hide", "alice", panevis.ErrTmuxFailed, "break-pane failed", "check tmux"} {
		if !strings.Contains(out, want) {
			t.Errorf("board view should surface the failure %q:\n%s", want, out)
		}
	}
	// A board refresh must NOT wipe the surfaced status (it survives the reload).
	m, _ = step(t, m, boardMsg{teammates: []teardown.Teammate{{Name: "alice", Team: "t1", PaneID: "%1"}}, epoch: m.boardEpoch})
	if m.boardStatus == "" {
		t.Fatal("a board refresh must not clear the hide/show status line")
	}
	// A subsequent OK show overwrites with an ok-styled confirmation.
	m, _ = step(t, m, paneVisMsg{res: panevis.Result{OK: true, Action: "show", Name: "alice"}})
	if m.boardStatusErr || !strings.Contains(m.boardStatus, "show alice") {
		t.Fatalf("ok show should set an ok status line: %q (err=%v)", m.boardStatus, m.boardStatusErr)
	}
	// Re-entering the board (esc to list, tab back) clears the stale status.
	mlist, _ := press(t, m, "esc")
	mboard, _ := press(t, mlist, "tab")
	if mboard.boardStatus != "" {
		t.Fatalf("re-entering the board should clear stale status, got %q", mboard.boardStatus)
	}
}

func TestWindowBounds(t *testing.T) {
	cases := []struct {
		name               string
		cursor, n, max     int
		wantStart, wantEnd int
	}{
		{"short list, no window", 0, 5, 12, 0, 5},
		{"long list, cursor top", 0, 50, 12, 0, 12},
		{"long list, cursor middle", 25, 50, 12, 19, 31},
		{"long list, cursor end", 49, 50, 12, 38, 50},
	}
	for _, c := range cases {
		s, e := windowBounds(c.cursor, c.n, c.max)
		if s != c.wantStart || e != c.wantEnd {
			t.Errorf("%s: windowBounds(%d,%d,%d) = [%d,%d), want [%d,%d)",
				c.name, c.cursor, c.n, c.max, s, e, c.wantStart, c.wantEnd)
		}
	}
}

func TestViewsRenderForEveryScreen(t *testing.T) {
	// Smoke test: every screen must render a non-empty frame without panic.
	screens := []screen{
		screenList, screenSpawn, screenPickTemplate,
		screenForm, screenModelPick, screenRemoveConfirm, screenResult, screenKeys,
	}
	for _, s := range screens {
		m := NewModel()
		m.loading = false // exercise the populated render paths, not "loading…"
		m.vendors = []userops.VendorView{{Name: "x", DefaultModel: "m", Enabled: true, ModelsCount: 1}}
		m.teammates = []teardown.Teammate{{Name: "a", Team: "t1", Vendor: "v", Model: "m", PaneID: "%1", PID: 1}}
		m.screen = s
		m.form = newAddForm(Templates[0])
		m.removeName = "x"
		m.result = "done"
		m.modelList = []models.Model{{ID: "x", OwnedBy: "y"}}
		m.keyVendor = "x"
		m.keys = []secrets.KeyEntry{{Label: "key1", Key: "sk-abcdef-123", Enabled: true}}
		if out := m.View(); out == "" {
			t.Errorf("screen %d rendered empty view", s)
		}
	}
}

// ---------------------------------------------------------------------------
// key manager (screenKeys)
// ---------------------------------------------------------------------------

// keysModel walks from the Vendors list into the EDIT form, focuses the "Manage
// API keys →" action row, opens screenKeys, and delivers the given key set
// (bypassing disk). It returns the model parked on screenKeys.
func keysModel(t *testing.T, vendor, rotation string, ks ...secrets.KeyEntry) Model {
	t.Helper()
	m := withVendors(t, userops.VendorView{
		Name: vendor, BaseURL: "https://api.example/anthropic",
		ModelsEndpoint: "https://api.example/v1/models", DefaultModel: "m", Enabled: true,
	})
	m, _ = press(t, m, "enter") // edit vendor -> form
	if m.screen != screenForm || m.formMode != modeEdit {
		t.Fatalf("expected edit form, screen=%d mode=%d", m.screen, m.formMode)
	}
	for i := 0; i < len(m.form.fields) && m.form.focusedKey() != "manage_keys"; i++ {
		m, _ = press(t, m, "down")
	}
	if m.form.focusedKey() != "manage_keys" {
		t.Fatalf("could not focus the manage_keys action row")
	}
	m, cmd := press(t, m, "enter")
	if m.screen != screenKeys {
		t.Fatalf("enter on manage_keys: screen=%d, want screenKeys", m.screen)
	}
	if cmd == nil {
		t.Fatalf("opening the key manager should dispatch a load cmd")
	}
	m, _ = step(t, m, keysetMsg{keys: ks, rotation: rotation})
	return m
}

func TestKeyManagerOpensAndLoads(t *testing.T) {
	m := keysModel(t, "glm", "round_robin",
		secrets.KeyEntry{Label: "primary", Key: "sk-aaa-111", Enabled: true},
		secrets.KeyEntry{Label: "backup", Key: "sk-bbb-222", Enabled: false},
	)
	if m.keyVendor != "glm" {
		t.Fatalf("keyVendor = %q, want glm", m.keyVendor)
	}
	if len(m.keys) != 2 || m.keyRotation != "round_robin" {
		t.Fatalf("keys=%d rotation=%q, want 2 + round_robin", len(m.keys), m.keyRotation)
	}
}

// TestKeyManagerNeverRendersPlaintext is the key-safety sentinel: a known
// plaintext key must NOT appear in the rendered view — only its mask.
func TestKeyManagerNeverRendersPlaintext(t *testing.T) {
	const sentinel = "sk-PLAINTEXT-SENTINEL-must-not-render-1234"
	m := keysModel(t, "glm", "round_robin",
		secrets.KeyEntry{Label: "primary", Key: sentinel, Enabled: true})
	out := m.View()
	if strings.Contains(out, sentinel) {
		t.Fatalf("key manager leaked the plaintext key into the view:\n%s", out)
	}
	if masked := secrets.MaskKey(sentinel); !strings.Contains(out, masked) {
		t.Fatalf("view should show the masked key %q\n%s", masked, out)
	}
	if !strings.Contains(out, "rotation: round_robin") {
		t.Fatalf("header should show the rotation strategy:\n%s", out)
	}
}

// TestKeyInputIsPasswordAndHidesPlaintext: the add/edit input must be an
// EchoPassword field and the typed key must never render in plaintext.
func TestKeyInputIsPasswordAndHidesPlaintext(t *testing.T) {
	m := keysModel(t, "glm", "off")
	m, _ = press(t, m, "a") // start add (cursor 0 == Add row with no keys)
	if !m.keyEditing {
		t.Fatal("'a' should enter the password input mode")
	}
	if m.keyInput.EchoMode != textinput.EchoPassword {
		t.Fatal("key input must use EchoPassword so typing is masked")
	}
	const typed = "sk-TYPED-SENTINEL-9999"
	m, _ = step(t, m, keyMsg(typed))
	if out := m.View(); strings.Contains(out, typed) {
		t.Fatalf("typed key leaked into the view:\n%s", out)
	}
}

func TestKeyManagerToggleEnabled(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa-111", Enabled: true},
		secrets.KeyEntry{Label: "b", Key: "sk-bbb-222", Enabled: false},
	)
	m, cmd := press(t, m, "space") // cursor 0 = key a
	if cmd == nil {
		t.Fatal("toggle should dispatch a save cmd")
	}
	if m.keys[0].Enabled {
		t.Fatal("space should toggle key0 enabled -> disabled")
	}
}

func TestKeyManagerAddKeyAppendsAndSaves(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa-111", Enabled: true})
	m, _ = press(t, m, "down") // cursor -> Add row (index 1)
	if m.keyCursor != 1 {
		t.Fatalf("cursor = %d, want 1 (Add row)", m.keyCursor)
	}
	m, _ = press(t, m, "enter") // start add
	if !m.keyEditing || m.keyEditIdx != -1 {
		t.Fatalf("enter on Add row: editing=%v idx=%d, want true,-1", m.keyEditing, m.keyEditIdx)
	}
	m, _ = step(t, m, keyMsg("sk-new-222"))
	m, cmd := press(t, m, "enter") // commit
	if cmd == nil {
		t.Fatal("committing an add should dispatch a save cmd")
	}
	if m.keyEditing {
		t.Fatal("committing should leave input mode")
	}
	if len(m.keys) != 2 || m.keys[1].Key != "sk-new-222" || !m.keys[1].Enabled {
		t.Fatalf("after add: keys=%+v, want appended enabled sk-new-222", m.keys)
	}
}

func TestKeyManagerEditKeyReplacesValueNoPrefill(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-old-111", Enabled: true})
	m, _ = press(t, m, "e") // edit key 0
	if !m.keyEditing || m.keyEditIdx != 0 {
		t.Fatalf("'e' on key 0: editing=%v idx=%d, want true,0", m.keyEditing, m.keyEditIdx)
	}
	// The input must NOT be prefilled with the existing key (no plaintext / no
	// length leak); the user types the replacement.
	if got := m.keyInput.Value(); got != "" {
		t.Fatalf("edit input prefilled with %q, want empty", got)
	}
	m, _ = step(t, m, keyMsg("sk-new-999"))
	m, cmd := press(t, m, "enter")
	if cmd == nil {
		t.Fatal("committing an edit should dispatch a save cmd")
	}
	if m.keys[0].Key != "sk-new-999" {
		t.Fatalf("edited key = %q, want sk-new-999", m.keys[0].Key)
	}
	if m.keys[0].Label != "a" || !m.keys[0].Enabled {
		t.Fatalf("edit should preserve label/enabled: %+v", m.keys[0])
	}
}

func TestKeyManagerDeleteKeyAndClamp(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa", Enabled: true},
		secrets.KeyEntry{Label: "b", Key: "sk-bbb", Enabled: true},
	)
	m, _ = press(t, m, "down") // cursor 1 = key b
	m, cmd := press(t, m, "d")
	if cmd == nil {
		t.Fatal("delete should dispatch a save cmd")
	}
	if len(m.keys) != 1 || m.keys[0].Label != "a" {
		t.Fatalf("after delete: keys=%+v, want [a]", m.keys)
	}
	if m.keyCursor != 1 { // now the Add row (len==1)
		t.Fatalf("cursor = %d, want 1 (clamped onto the Add row)", m.keyCursor)
	}
}

func TestKeyManagerEmptyKeyRejected(t *testing.T) {
	m := keysModel(t, "glm", "off")
	m, _ = press(t, m, "a")        // start add
	m, cmd := press(t, m, "enter") // commit empty
	if cmd != nil {
		t.Fatal("committing an empty key should not dispatch a save cmd")
	}
	if !m.keyEditing {
		t.Fatal("empty commit should stay in input mode")
	}
	if m.keyErr == "" {
		t.Fatal("empty commit should set an inline error")
	}
	if len(m.keys) != 0 {
		t.Fatalf("empty commit should not append, keys=%d", len(m.keys))
	}
}

func TestKeyManagerCursorClamp(t *testing.T) {
	m := keysModel(t, "glm", "off", secrets.KeyEntry{Key: "k1", Enabled: true})
	m, _ = press(t, m, "up") // at top
	if m.keyCursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0", m.keyCursor)
	}
	for i := 0; i < 5; i++ {
		m, _ = press(t, m, "down")
	}
	if m.keyCursor != 1 { // 1 key + Add row -> max index 1
		t.Fatalf("down past end: cursor = %d, want 1 (Add row)", m.keyCursor)
	}
}

func TestKeyManagerCycleRotation(t *testing.T) {
	m := keysModel(t, "glm", "off", secrets.KeyEntry{Key: "k1", Enabled: true})
	m, cmd := press(t, m, "t")
	if cmd == nil {
		t.Fatal("'t' should dispatch a rotation-set cmd")
	}
	m, _ = step(t, m, rotationSetMsg{rotation: "round_robin"})
	if m.keyRotation != "round_robin" {
		t.Fatalf("rotation = %q, want round_robin", m.keyRotation)
	}
	if !strings.Contains(m.View(), "rotation: round_robin") {
		t.Fatalf("header should reflect the new rotation:\n%s", m.View())
	}
}

func TestNextRotationCycle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "round_robin"},
		{"off", "round_robin"},
		{"round_robin", "random"},
		{"random", "off"},
		// an invalid stored value RESETS to off (explicit recovery), it does
		// NOT silently advance via off.Next().
		{"bogus", "off"},
	}
	for _, c := range cases {
		if got := nextRotation(c.in); got != c.want {
			t.Errorf("nextRotation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNextRotation_ParserPath confirms nextRotation routes through
// config.ParseKeyRotation rather than its own string switch, AND an
// unrecognized value explicitly RESETS to off (not silently advanced to
// round_robin).
func TestNextRotation_ParserPath(t *testing.T) {
	// Case-sensitive typos must be REJECTED by the parser and reset to "off",
	// not silently treated as the matching valid value AND not advanced to the
	// next strategy in the cycle.
	if got := nextRotation("Off"); got != "off" {
		t.Errorf("nextRotation(%q) = %q, want off (invalid → reset)", "Off", got)
	}
	if got := nextRotation("round-robin"); got != "off" {
		t.Errorf("nextRotation(%q) = %q, want off (invalid → reset)", "round-robin", got)
	}
}

func TestKeyManagerEscReturnsToForm(t *testing.T) {
	m := keysModel(t, "glm", "off", secrets.KeyEntry{Key: "k1", Enabled: true})
	m, _ = press(t, m, "esc")
	if m.screen != screenForm {
		t.Fatalf("esc should return to the edit form, screen = %d", m.screen)
	}
}

func TestKeyManagerLoadErrorSurfaces(t *testing.T) {
	m := keysModel(t, "glm", "off")
	m, _ = step(t, m, keysetMsg{err: errors.New("corrupt keys.json")})
	if m.keyErr == "" {
		t.Fatal("a load error should surface as keyErr")
	}
	if m.keys != nil {
		t.Fatalf("load error should clear keys, got %+v", m.keys)
	}
}

// TestHandlersRegistry_AllScreensRegistered is the enumeration guard: every
// screen constant must have an entry in the package-level `handlers` map.
// Adding a new screen without registering it here would silently produce a TUI
// with an unmapped state.
func TestHandlersRegistry_AllScreensRegistered(t *testing.T) {
	for _, s := range allScreens() {
		h, ok := handlers[s]
		if !ok {
			t.Errorf("screen %d missing from handlers registry", s)
			continue
		}
		if h.update == nil {
			t.Errorf("screen %d: update is nil", s)
		}
		if h.view == nil {
			t.Errorf("screen %d: view is nil", s)
		}
	}
	// And no extras.
	have := map[screen]struct{}{}
	for _, s := range allScreens() {
		have[s] = struct{}{}
	}
	for s := range handlers {
		if _, ok := have[s]; !ok {
			t.Errorf("handlers has screen %d that is not in allScreens()", s)
		}
	}
}

// TestAsyncMsg_NonOwningScreenDrop is the ownership regression: a screen-owned
// async msg arriving while the model is on a DIFFERENT screen must be dropped
// without mutating model state.
func TestAsyncMsg_NonOwningScreenDrop(t *testing.T) {
	// Model on screenList; receive a modelsMsg (owned by screenModelPick).
	m := withVendors(t, userops.VendorView{Name: "glm"})
	if m.screen != screenList {
		t.Fatalf("setup: screen = %d, want screenList", m.screen)
	}
	stale := modelsMsg{models: []models.Model{{ID: "stale-model"}}, err: nil}
	m, _ = step(t, m, stale)

	if len(m.modelList) != 0 {
		t.Fatalf("non-owning screen received modelsMsg: modelList = %+v, want empty",
			m.modelList)
	}
	if m.modelsErr != nil {
		t.Fatalf("non-owning screen overwrote modelsErr = %v", m.modelsErr)
	}
}

// TestAsyncMsg_OwningScreenAccepts verifies the inverse: an owned msg DOES
// reach the model when on the matching screen.
func TestAsyncMsg_OwningScreenAccepts(t *testing.T) {
	m := withVendors(t, userops.VendorView{Name: "glm"})
	// Navigate into the model picker via the same flow real users take.
	// Simpler: just set the screen directly; we're testing the dispatch, not
	// the entrypoint chain.
	m.screen = screenModelPick
	fresh := modelsMsg{models: []models.Model{{ID: "live-model"}}, err: nil}
	m, _ = step(t, m, fresh)
	if len(m.modelList) != 1 || m.modelList[0].ID != "live-model" {
		t.Fatalf("owning screen ignored modelsMsg: modelList = %+v", m.modelList)
	}
}

// TestAsyncMsg_VendorsMsgNonOwningScreenDrop: a vendor-list result arriving
// while the user has navigated off screenList (e.g. to the board) must NOT
// clobber m.loading / m.vendors / m.vendorsErr — otherwise a slow userops.List
// could blank the board's "loading…" while it is still mid-discover.
func TestAsyncMsg_VendorsMsgNonOwningScreenDrop(t *testing.T) {
	m := withVendors(t, userops.VendorView{Name: "live"})
	// Navigate to the board (epoch bumps to 1, loading=true). The board hasn't
	// resolved yet, so m.loading must STAY true after a stale vendorsMsg.
	m, _ = press(t, m, "tab")
	if m.screen != screenSpawn || !m.loading {
		t.Fatalf("setup: screen=%d loading=%v, want screenSpawn + loading=true", m.screen, m.loading)
	}
	wantVendors := m.vendors
	stale := vendorsMsg{vendors: []userops.VendorView{{Name: "stale"}}, err: errors.New("late err")}
	m, _ = step(t, m, stale)
	if !m.loading {
		t.Fatal("non-owning screen received vendorsMsg: m.loading flipped to false")
	}
	if len(m.vendors) != len(wantVendors) || (len(m.vendors) > 0 && m.vendors[0].Name != "live") {
		t.Fatalf("non-owning screen overwrote m.vendors = %+v, want unchanged %+v", m.vendors, wantVendors)
	}
	if m.vendorsErr != nil {
		t.Fatalf("non-owning screen overwrote m.vendorsErr = %v", m.vendorsErr)
	}
}

// TestAsyncMsg_BoardMsgNonOwningScreenDrop covers BOTH ways a board refresh can
// be stale:
//
//  1. screen mismatch — user already left the board (back to screenList);
//  2. epoch mismatch — user re-entered the board, bumping boardEpoch, while a
//     refresh scheduled by the prior visit is still in flight.
//
// In both cases the handler must drop the message without touching loading /
// teammates / spawnErr.
func TestAsyncMsg_BoardMsgNonOwningScreenDrop(t *testing.T) {
	t.Run("screen mismatch", func(t *testing.T) {
		// Park the model on screenList (the hub). A boardMsg arriving here
		// from a previous board visit must not mutate state.
		m := withVendors(t, userops.VendorView{Name: "glm"})
		if m.screen != screenList {
			t.Fatalf("setup: screen=%d, want screenList", m.screen)
		}
		// Seed a known marker so we can detect a leak via the teammates field.
		stale := boardMsg{
			teammates: []teardown.Teammate{{Name: "leaked", Team: "t"}},
			epoch:     m.boardEpoch + 1, // even matching epoch must not save it
		}
		m, _ = step(t, m, stale)
		for _, tm := range m.teammates {
			if tm.Name == "leaked" {
				t.Fatalf("non-owning screen received boardMsg: leaked teammate = %+v", tm)
			}
		}
	})

	t.Run("epoch mismatch", func(t *testing.T) {
		// User entered the board (epoch 1), then re-entered (epoch 2). A
		// boardMsg stamped with epoch 1 must be dropped — the gate keeps the
		// fresh visit's loading=true and empty teammates list.
		m := withVendors(t, userops.VendorView{Name: "glm"})
		m, _ = press(t, m, "tab") // enter -> epoch 1
		if m.boardEpoch != 1 || !m.loading {
			t.Fatalf("setup: epoch=%d loading=%v, want 1 + loading", m.boardEpoch, m.loading)
		}
		// Re-enter without resolving the first refresh: esc back to list and
		// tab forward again (epoch bumps to 2). loading stays true.
		m, _ = press(t, m, "esc") // -> list
		m, _ = press(t, m, "tab") // -> board, epoch=2
		if m.boardEpoch != 2 || !m.loading {
			t.Fatalf("after re-entry: epoch=%d loading=%v, want 2 + loading", m.boardEpoch, m.loading)
		}
		stale := boardMsg{
			teammates: []teardown.Teammate{{Name: "leaked", Team: "t"}},
			epoch:     1, // from the FIRST board visit; must be dropped
		}
		m, _ = step(t, m, stale)
		if !m.loading {
			t.Fatal("stale-epoch boardMsg flipped m.loading to false")
		}
		if len(m.teammates) != 0 {
			t.Fatalf("stale-epoch boardMsg leaked teammates: %+v", m.teammates)
		}
	})

	t.Run("matching epoch + owning screen accepts", func(t *testing.T) {
		// Sanity: when both gates pass, the refresh DOES land — proving the
		// drop above is not just "always dropped".
		m := withVendors(t, userops.VendorView{Name: "glm"})
		m, _ = press(t, m, "tab")
		fresh := boardMsg{
			teammates: []teardown.Teammate{{Name: "ok", Team: "t1"}},
			epoch:     m.boardEpoch,
		}
		m, _ = step(t, m, fresh)
		if m.loading {
			t.Fatal("matching boardMsg should clear m.loading")
		}
		if len(m.teammates) != 1 || m.teammates[0].Name != "ok" {
			t.Fatalf("matching boardMsg dropped: teammates = %+v", m.teammates)
		}
	})
}

// TestLoadKeysetCmd_SurfacesConfigLoadError: when vendors.toml is corrupt
// (config.Load returns an error), the resulting keysetMsg must carry that error
// so it renders into m.keyErr in the key manager instead of being silently
// swallowed (which left rotation "").
//
// The setup uses a valid keys.json (so secrets.LoadKeySet returns nil err)
// alongside a malformed vendors.toml; the config.Load error is the ONLY error
// path, so the assertion is unambiguous.
func TestLoadKeysetCmd_SurfacesConfigLoadError(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // never fall through to a real $HOME

	appDir := filepath.Join(xdg, "cc-fleet")
	if err := os.MkdirAll(filepath.Join(appDir, "secrets"), 0o700); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	// Valid keys.json so LoadKeySet succeeds.
	keysJSON := []byte(`[{"key":"sk-aaa","enabled":true}]`)
	if err := os.WriteFile(filepath.Join(appDir, "secrets", "glm.keys.json"), keysJSON, 0o600); err != nil {
		t.Fatalf("write keys.json: %v", err)
	}
	// Malformed vendors.toml so config.Load returns an error.
	if err := os.WriteFile(filepath.Join(appDir, "vendors.toml"), []byte("this is not [valid toml"), 0o600); err != nil {
		t.Fatalf("write corrupt vendors.toml: %v", err)
	}

	cmd := loadKeysetCmd("glm")
	if cmd == nil {
		t.Fatal("loadKeysetCmd returned nil")
	}
	raw := cmd()
	msg, ok := raw.(keysetMsg)
	if !ok {
		t.Fatalf("loadKeysetCmd returned %T, want keysetMsg", raw)
	}
	if msg.err == nil {
		t.Fatal("keysetMsg.err is nil; a corrupt vendors.toml must surface to the TUI")
	}
	// keys still parsed; only the rotation lookup failed.
	if len(msg.keys) != 1 {
		t.Fatalf("LoadKeySet should still parse: msg.keys = %+v", msg.keys)
	}
}

// TestKeyMgr_ToggleFailureReloadsFromDisk: when SaveKeySet fails (forced via the
// seam), the keysSavedMsg{err:...} handler must reload the on-disk keys.json so
// the in-memory m.keys converges back to disk truth. The user's TUI cannot show
// "key X enabled" while keyget would hand out the previous (disabled) state.
func TestKeyMgr_ToggleFailureReloadsFromDisk(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa", Enabled: true},
	)

	// Press space to toggle key 0 enabled -> disabled in memory. The save
	// dispatch will return a forced error.
	m, _ = press(t, m, "space")
	if m.keys[0].Enabled {
		t.Fatal("space should toggle enabled in memory")
	}

	// Now deliver a forced keysSavedMsg{err: ...}; the handler must:
	//   (a) set keyErr,
	//   (b) return a reload command (loadKeysetCmd) so the next msg pump
	//       replaces m.keys with disk truth.
	m, cmd := step(t, m, keysSavedMsg{err: errors.New("save failed")})
	if m.keyErr == "" {
		t.Fatal("keyErr should be set on save failure")
	}
	if cmd == nil {
		t.Fatal("save failure should dispatch a reload cmd (loadKeysetCmd)")
	}

	// Simulate the reload completing: deliver a keysetMsg with the disk's
	// (unchanged) state — key still enabled. The UI must converge back.
	disk := []secrets.KeyEntry{{Label: "a", Key: "sk-aaa", Enabled: true}}
	m, _ = step(t, m, keysetMsg{keys: disk, rotation: "off"})

	if len(m.keys) != 1 || !m.keys[0].Enabled {
		t.Fatalf("after reload: m.keys = %+v, want one enabled key", m.keys)
	}
}

// TestKeyMgr_DeleteFailureReloadsFromDisk: the same reload-from-disk guarantee
// applies to a failed delete, exercising the same keysSavedMsg handler.
func TestKeyMgr_DeleteFailureReloadsFromDisk(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa", Enabled: true},
		secrets.KeyEntry{Label: "b", Key: "sk-bbb", Enabled: true},
	)

	m, _ = press(t, m, "down") // cursor 1 = key b
	m, _ = press(t, m, "d")    // delete key b in memory
	if len(m.keys) != 1 {
		t.Fatalf("delete: m.keys len = %d, want 1", len(m.keys))
	}

	m, cmd := step(t, m, keysSavedMsg{err: errors.New("save failed")})
	if cmd == nil {
		t.Fatal("save failure on delete should dispatch a reload cmd")
	}
	if m.keyErr == "" {
		t.Fatal("save failure should surface as keyErr")
	}

	// Reload: disk still has both keys; UI must converge.
	disk := []secrets.KeyEntry{
		{Label: "a", Key: "sk-aaa", Enabled: true},
		{Label: "b", Key: "sk-bbb", Enabled: true},
	}
	m, _ = step(t, m, keysetMsg{keys: disk, rotation: "off"})
	if len(m.keys) != 2 {
		t.Fatalf("after reload: m.keys = %+v, want both keys restored from disk", m.keys)
	}
}

// TestKeyMgr_SaveSuccessDoesNotReload: a successful save must NOT dispatch a
// reload — the in-memory state is already consistent with disk.
func TestKeyMgr_SaveSuccessDoesNotReload(t *testing.T) {
	m := keysModel(t, "glm", "off",
		secrets.KeyEntry{Label: "a", Key: "sk-aaa", Enabled: true},
	)
	m, _ = press(t, m, "space") // toggle disable
	m, cmd := step(t, m, keysSavedMsg{err: nil})
	if cmd != nil {
		t.Fatalf("save success should NOT reload; got cmd=%v", cmd)
	}
	if m.keyErr != "" {
		t.Fatalf("save success should clear keyErr; got %q", m.keyErr)
	}
}

// ---------------------------------------------------------------------------
// Dynamic Workflows (run boxes folded into the Agents Board, screenSpawn)
// ---------------------------------------------------------------------------

// TestGroupByRun_PhaseOrderAndRunOrder: phases render in manifest order first,
// then any phase observed on a job but absent from the manifest appended in
// first-seen order; runs render newest-first by StartedAt.
func TestGroupByRun_PhaseOrderAndRunOrder(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "run-old", Name: "old", StartedAt: "2026-05-01T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "plan"}, {Title: "build"}}},
		{RunID: "run-new", Name: "new", StartedAt: "2026-05-10T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "design"}}},
	}
	jobs := []subagent.Result{
		// run-old: a job in a declared phase + a job in a manifest-absent extra.
		{RunID: "run-old", Phase: "build", Label: "b1", StartedAt: "2026-05-01T01:00:00Z"},
		{RunID: "run-old", Phase: "ship", Label: "s1", StartedAt: "2026-05-01T02:00:00Z"},
		{RunID: "run-new", Phase: "design", Label: "d1", StartedAt: "2026-05-10T01:00:00Z"},
	}
	groups := groupByRun(jobs, runs)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	// Newest-first: run-new before run-old.
	if groups[0].runID != "run-new" || groups[1].runID != "run-old" {
		t.Fatalf("run order = [%s, %s], want [run-new, run-old]", groups[0].runID, groups[1].runID)
	}
	// run-old phases: manifest order (plan, build) then observed extra (ship).
	old := groups[1]
	gotPhases := []string{}
	for _, p := range old.phases {
		gotPhases = append(gotPhases, p.title)
	}
	want := []string{"plan", "build", "ship"}
	if strings.Join(gotPhases, ",") != strings.Join(want, ",") {
		t.Fatalf("run-old phase order = %v, want %v", gotPhases, want)
	}
	// "plan" was declared but has no job; "build" has one; "ship" (extra) has one.
	if len(old.phases[0].jobs) != 0 {
		t.Fatalf("declared-but-jobless phase 'plan' should have 0 jobs, got %d", len(old.phases[0].jobs))
	}
	if len(old.phases[1].jobs) != 1 || len(old.phases[2].jobs) != 1 {
		t.Fatalf("phases build/ship should each have 1 job, got %d/%d",
			len(old.phases[1].jobs), len(old.phases[2].jobs))
	}
}

// TestGroupByRun_ManifestOnlyRunYieldsPhaseSkeleton: a run with a manifest but
// zero jobs still produces its declared phase groups (the phase plan), so a
// freshly-created run shows its skeleton.
func TestGroupByRun_ManifestOnlyRunYieldsPhaseSkeleton(t *testing.T) {
	runs := []subagent.WorkflowRun{
		{RunID: "run-x", Name: "x", StartedAt: "2026-05-05T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "alpha"}, {Title: "beta"}}},
	}
	groups := groupByRun(nil, runs)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	g := groups[0]
	if len(g.phases) != 2 || g.phases[0].title != "alpha" || g.phases[1].title != "beta" {
		t.Fatalf("manifest-only run phases = %+v, want [alpha beta] skeleton", g.phases)
	}
	for _, p := range g.phases {
		if len(p.jobs) != 0 {
			t.Fatalf("manifest-only run phase %q should have 0 jobs, got %d", p.title, len(p.jobs))
		}
	}
}

// TestGroupByRun_NoManifestRunOrdersByEarliestJob: a run whose jobs have no
// manifest (GC'd / never created) gets an empty name, phases in first-seen order,
// and sorts by its earliest job StartedAt.
func TestGroupByRun_NoManifestRunOrdersByEarliestJob(t *testing.T) {
	jobs := []subagent.Result{
		{RunID: "orphan", Phase: "second", Label: "j2", StartedAt: "2026-05-02T00:00:00Z"},
		{RunID: "orphan", Phase: "first", Label: "j1", StartedAt: "2026-05-01T00:00:00Z"},
	}
	runs := []subagent.WorkflowRun{
		{RunID: "manifested", Name: "m", StartedAt: "2026-05-03T00:00:00Z",
			Phases: []subagent.RunPhase{{Title: "p"}}},
	}
	groups := groupByRun(jobs, runs)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	// manifested (2026-05-03) is newest → first; orphan (earliest job 2026-05-01) → last.
	if groups[0].runID != "manifested" || groups[1].runID != "orphan" {
		t.Fatalf("run order = [%s, %s], want [manifested, orphan]", groups[0].runID, groups[1].runID)
	}
	orphan := groups[1]
	if orphan.name != "" {
		t.Fatalf("no-manifest run should have empty name, got %q", orphan.name)
	}
	// Phases in first-seen order: "second" then "first".
	if len(orphan.phases) != 2 || orphan.phases[0].title != "second" || orphan.phases[1].title != "first" {
		t.Fatalf("orphan phase order = %+v, want [second first] (first-seen)", orphan.phases)
	}
}

// TestPartition_RunTaggedJobNotOnSpawnBoard: a RunID-tagged job feeds the session's run
// box, never the Subagents box — its session tree carries only RunID == "" jobs as standalone
// subagents. The tagged job's id must not appear among the Subagents rows (it lives under the
// Dynamic Workflows box instead); the plain job's does.
func TestPartition_RunTaggedJobNotOnSpawnBoard(t *testing.T) {
	tagged := subagent.Result{JobID: "tagged00", RunID: "run-1", Phase: "build", Label: "b1",
		Vendor: "glm", Status: "running", StartedAt: "2026-05-01T00:00:00Z", LeadSessionID: "s"}
	plain := subagent.Result{JobID: "plain000", Vendor: "kimi", Status: "done",
		StartedAt: "2026-05-01T00:00:00Z", LeadSessionID: "s"}
	runs := []subagent.WorkflowRun{{RunID: "run-1", Name: "sweep", SessionID: "s", StartedAt: "2026-05-01T00:00:00Z"}}
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{jobs: []subagent.Result{tagged, plain}, runs: runs, epoch: m.boardEpoch})
	out := m.View()
	if strings.Contains(out, "tagged00") {
		t.Errorf("RunID-tagged job leaked into the Subagents box on the Agents Board:\n%s", out)
	}
	if !strings.Contains(out, "plain000") {
		t.Errorf("ungrouped job should appear in the Subagents box:\n%s", out)
	}
	// The standalone (RunID == "") jobs are the only entries in each session's Subagents box;
	// the tagged job must instead live among the session's runs.
	for _, s := range m.asSessions() {
		for _, j := range s.jobs {
			if j.RunID != "" {
				t.Errorf("session tree carried a RunID-tagged job in its Subagents box: %+v", j)
			}
		}
	}
	if got := len(m.workflowJobs); got != 1 {
		t.Errorf("the tagged job should be the only workflow leaf, got %d", got)
	}
}

// TestWfRefresh_StaleEpochDropped: a wfRefreshMsg whose epoch != the model's current
// boardEpoch (a prior visit's in-flight light refresh) is dropped — the fresh visit's
// workflow data survives unchanged.
func TestWfRefresh_StaleEpochDropped(t *testing.T) {
	m := boardModel(t, nil, nil)
	m, _ = step(t, m, boardMsg{
		jobs:  []subagent.Result{{RunID: "run-z", Phase: "p", JobID: "ok000000", Label: "agent-a"}},
		runs:  []subagent.WorkflowRun{{RunID: "run-z", Name: "z", StartedAt: "2026-05-01T00:00:00Z"}},
		epoch: m.boardEpoch,
	})
	before := len(m.workflowJobs)
	stale := wfRefreshMsg{
		jobs:  nil,
		runs:  nil,
		epoch: m.boardEpoch - 1, // from a prior visit; must be dropped
	}
	m, _ = step(t, m, stale)
	if len(m.workflowJobs) != before {
		t.Fatalf("a stale-epoch wfRefreshMsg must be dropped, jobs went %d → %d", before, len(m.workflowJobs))
	}
	// A matching-epoch refresh DOES land (sanity): it replaces the workflow halves.
	fresh := wfRefreshMsg{
		jobs:  []subagent.Result{{RunID: "run-z", Phase: "p", JobID: "ok000000", Label: "agent-b"}},
		runs:  []subagent.WorkflowRun{{RunID: "run-z", Name: "z", StartedAt: "2026-05-01T00:00:00Z"}},
		epoch: m.boardEpoch,
	}
	m, _ = step(t, m, fresh)
	if len(m.workflowJobs) != 1 || m.workflowJobs[0].Label != "agent-b" {
		t.Fatalf("matching wfRefreshMsg dropped: jobs=%+v", m.workflowJobs)
	}
}

// TestWfLiveChain_StartsReschedulesStops: a boardMsg whose data has a running RunID-tagged
// leaf starts the 500ms light chain (wfLiveOn true + a non-nil cmd); a current-epoch
// wfLiveTickMsg with something still running reschedules; one with nothing running clears
// wfLiveOn and returns no cmd; a stale-epoch tick is dropped.
func TestWfLiveChain_StartsReschedulesStops(t *testing.T) {
	runningLeaf := []subagent.Result{{RunID: "run-z", Phase: "p", Label: "a", JobID: "j1", Status: "running",
		StartedAt: "2026-05-01T00:00:00Z"}}
	runs := []subagent.WorkflowRun{{RunID: "run-z", Name: "z", Status: "running", StartedAt: "2026-05-01T00:00:00Z"}}

	m := boardModel(t, nil, nil)
	m, cmd := step(t, m, boardMsg{jobs: runningLeaf, runs: runs, epoch: m.boardEpoch})
	if !m.wfLiveOn || cmd == nil {
		t.Fatalf("a running leaf should start the light chain: wfLiveOn=%v cmd=%v", m.wfLiveOn, cmd)
	}
	// A current-epoch tick while a leaf still runs reschedules.
	if _, c := step(t, m, wfLiveTickMsg{epoch: m.boardEpoch}); c == nil {
		t.Fatal("a current-epoch live tick with a running leaf should reschedule (non-nil cmd)")
	}
	// A stale-epoch tick is dropped (no reschedule).
	if _, c := step(t, m, wfLiveTickMsg{epoch: m.boardEpoch - 1}); c != nil {
		t.Fatal("a stale-epoch live tick must not reschedule")
	}
	// Once nothing runs, the tick clears the flag and stops the chain.
	doneLeaf := []subagent.Result{{RunID: "run-z", Phase: "p", Label: "a", JobID: "j1", Status: "done",
		StartedAt: "2026-05-01T00:00:00Z", NumTurns: 1}}
	m2, _ := step(t, m, wfRefreshMsg{jobs: doneLeaf, runs: runs, epoch: m.boardEpoch})
	m2, c := step(t, m2, wfLiveTickMsg{epoch: m2.boardEpoch})
	if m2.wfLiveOn || c != nil {
		t.Fatalf("with nothing running the chain should stop: wfLiveOn=%v cmd=%v", m2.wfLiveOn, c)
	}
}

// TestRunLabel_SanitizesRunID: ids.ValidateJobID permits non-whitespace control
// runes in a run id, so the board scrubs it through CleanTitle before rendering —
// a run id carrying an ANSI escape must not reach the terminal raw.
func TestRunLabel_SanitizesRunID(t *testing.T) {
	m := boardModel(t, nil, nil)
	jobs := []subagent.Result{{RunID: "\x1b[31mevil", Phase: "p", Label: "a", StartedAt: "2026-05-01T00:00:00Z"}}
	groups := groupByRun(jobs, nil)
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if label := m.runLabel(groups[0]); strings.ContainsRune(label, '\x1b') {
		t.Fatalf("runLabel leaked a raw ESC byte from the run id: %q", label)
	}
}

// endedMate is a synthetic ended teammate (the shape synthesizeEnded produces).
func endedMate(team, name, session string) teardown.Teammate {
	return teardown.Teammate{Team: team, Name: name, LeadSessionID: session, Status: endedStatus}
}

// endedBoard enters the board with one ended team (rendered from its history record)
// plus any live teammates, stamping endedSeen so the card + rail recognize it.
func endedBoard(t *testing.T, ended map[string]time.Time, tms []teardown.Teammate) Model {
	t.Helper()
	m := withVendors(t, userops.VendorView{Name: "glm"})
	m, _ = press(t, m, "tab")
	m, _ = step(t, m, boardMsg{teammates: tms, endedSeen: ended, epoch: m.boardEpoch})
	return m
}

// TestBoardEndedTeamRenders: an ended team renders faint with the word `ended`, the
// card shows "last seen", and the team is excluded from the live teammate counts.
func TestBoardEndedTeamRenders(t *testing.T) {
	seen := time.Date(2026, 6, 7, 14, 30, 0, 0, time.UTC)
	// Two teams in one session keep the board at the boxes level (so the team rail
	// renders): one live, one ended.
	m := endedBoard(t, map[string]time.Time{"gone": seen}, []teardown.Teammate{
		{Team: "live", Name: "alice", PaneID: "%1", Status: "ok", LeadSessionID: "s"},
		endedMate("gone", "bob", "s"),
	})
	if m.asMode != asModeBoxes {
		t.Fatalf("two-team session should park at boxes, mode=%d", m.asMode)
	}
	out := m.View()
	// The session counts cover only the LIVE team's member.
	if !strings.Contains(out, "1 teammates") {
		t.Errorf("ended member inflated the teammate count:\n%s", out)
	}
	// The ended team's rail row reads "ended · N members", not an okN/N.
	if !strings.Contains(out, endedStatus+" · 1 members") {
		t.Errorf("ended team rail row missing `ended · 1 members`:\n%s", out)
	}
	// Descend onto the ended team to render its member card with the last-seen line.
	m.asBoxCursor = 1 // teams render after 0 runs: row 0 = live, row 1 = gone (encounter order)
	mc, _ := press(t, m, "enter")
	t2, ok := mc.selectedTeammate()
	if !ok || t2.Status != endedStatus {
		t.Fatalf("descend should land on the ended member, got %+v ok=%v", t2, ok)
	}
	card := strings.Join(mc.teammateDetailLines(t2, mc.asEntityRightWidth()), "\n")
	if !strings.Contains(card, endedStatus) || !strings.Contains(card, "last seen 06-07 14:30") {
		t.Errorf("ended card missing `ended` / last-seen line:\n%s", card)
	}
}

// TestBoardEndedTeamHideShowNoOp: h/s on an ended member issue no command (no live pane).
func TestBoardEndedTeamHideShowNoOp(t *testing.T) {
	m := endedBoard(t, map[string]time.Time{"gone": time.Now()},
		[]teardown.Teammate{endedMate("gone", "bob", "s")})
	// An ended team never skips the boxes level (its d×2 record delete lives there);
	// ⏎ on the team row opens the member view.
	if m.asMode != asModeBoxes {
		t.Fatalf("single ended team should keep the boxes level, mode=%d", m.asMode)
	}
	m, _ = press(t, m, "enter")
	if m.asMode != asModeEntity {
		t.Fatalf("⏎ on the ended team row should open its member view, mode=%d", m.asMode)
	}
	t2, ok := m.selectedTeammate()
	if !ok || t2.Status != endedStatus {
		t.Fatalf("cursor not on the ended member: %+v ok=%v", t2, ok)
	}
	if _, cmd := press(t, m, "h"); cmd != nil {
		t.Error("h on an ended row should be a no-op")
	}
	if _, cmd := press(t, m, "s"); cmd != nil {
		t.Error("s on an ended row should be a no-op")
	}
}

// TestBoardEndedTeamDelete: d on an ended team row arms the prompt; a second d dispatches
// the record delete (and a live team row ignores d).
func TestBoardEndedTeamDelete(t *testing.T) {
	m := endedBoard(t, map[string]time.Time{"gone": time.Now()}, []teardown.Teammate{
		{Team: "live", Name: "alice", PaneID: "%1", Status: "ok", LeadSessionID: "s"},
		endedMate("gone", "bob", "s"),
	})
	if m.asMode != asModeBoxes {
		t.Fatalf("setup: mode=%d, want boxes", m.asMode)
	}
	// Cursor on the LIVE team row (row 0): d must NOT arm.
	m.asBoxCursor = 0
	m1, cmd := press(t, m, "d")
	if cmd != nil || m1.teamHistDeleteArm != "" || m1.boardStatus == teamHistDeleteArmPrompt {
		t.Fatalf("d on a live team row armed a delete: arm=%q status=%q cmd=%v", m1.teamHistDeleteArm, m1.boardStatus, cmd)
	}
	// Cursor on the ENDED team row (row 1): first d arms, second d dispatches.
	m.asBoxCursor = 1
	m2, cmd := press(t, m, "d")
	if cmd != nil {
		t.Fatal("first d should only arm, not dispatch")
	}
	if m2.teamHistDeleteArm != "gone" || m2.boardStatus != teamHistDeleteArmPrompt {
		t.Fatalf("first d did not arm: arm=%q status=%q", m2.teamHistDeleteArm, m2.boardStatus)
	}
	m3, cmd := press(t, m2, "d")
	if cmd == nil {
		t.Fatal("second d should dispatch the delete (non-nil cmd)")
	}
	if m3.teamHistDeleteArm != "" {
		t.Errorf("arm not cleared after dispatch: %q", m3.teamHistDeleteArm)
	}
	// A non-d key disarms a pending delete.
	m4, _ := press(t, m2, "j")
	if m4.teamHistDeleteArm != "" || m4.boardStatus == teamHistDeleteArmPrompt {
		t.Errorf("a non-d key did not disarm: arm=%q status=%q", m4.teamHistDeleteArm, m4.boardStatus)
	}
}

// TestSynthesizeEndedBackfillsCwd: a record's member cwd back-fills the session meta when
// live resolution left it empty, so the ended member's card resolves its transcript path.
func TestSynthesizeEndedBackfillsCwd(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := teamhist.Upsert(
		[]teardown.Teammate{{Team: "gone", Name: "bob", LeadSessionID: "s1"}},
		func(string) string { return "/recorded/dir" },
	); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// meta starts with NO cwd for s1 — the live resolution found nothing.
	meta := map[string]sessiontitle.Meta{}
	ended, seen := synthesizeEnded(nil, meta)
	if len(ended) != 1 || ended[0].Status != endedStatus || ended[0].Name != "bob" {
		t.Fatalf("synthesizeEnded = %+v, want one ended bob", ended)
	}
	if meta["s1"].Cwd != "/recorded/dir" {
		t.Errorf("cwd not back-filled into session meta: %q", meta["s1"].Cwd)
	}
	if _, ok := seen["gone"]; !ok {
		t.Error("endedSeen missing the gone team")
	}
	// A live team of the same name shadows the record (no ended row).
	ended2, _ := synthesizeEnded([]teardown.Teammate{{Team: "gone", Name: "x"}}, map[string]sessiontitle.Meta{})
	if len(ended2) != 0 {
		t.Errorf("a live team should shadow its record, got %+v", ended2)
	}
}

// TestBoardEndedTeamKeySafe: a canary planted in a record's member string is scrubbed
// through CleanTitle on every board surface it can reach. The team name itself stays
// path-safe (it becomes a filename, ids-validated on read), so the canary rides the
// model string, which ids never constrains.
func TestBoardEndedTeamKeySafe(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const modelCanary = "model\x1b[31mEVIL"
	if err := teamhist.Upsert(
		[]teardown.Teammate{{Team: "gone", Name: "bob", Model: modelCanary, LeadSessionID: "s"}},
		func(string) string { return "" },
	); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	ended, seen := synthesizeEnded(nil, map[string]sessiontitle.Meta{})
	if len(ended) != 1 {
		t.Fatalf("expected one ended row, got %+v", ended)
	}
	m := endedBoard(t, seen, ended)
	mc, _ := press(t, m, "enter") // single ended team → straight to its member card
	t2, _ := mc.selectedTeammate()
	for name, out := range map[string]string{
		"view": mc.View(),
		"card": strings.Join(mc.teammateDetailLines(t2, mc.asEntityRightWidth()), "\n"),
	} {
		if strings.ContainsRune(out, '\x1b') {
			t.Errorf("%s leaked a raw ESC byte from a record-sourced string:\n%q", name, out)
		}
	}
}
