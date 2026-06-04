package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/models"
	"github.com/ethanhq/cc-fleet/internal/panevis"
	"github.com/ethanhq/cc-fleet/internal/secrets"
	"github.com/ethanhq/cc-fleet/internal/subagent"
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

// TestTabTogglesSpawnStatus: tab from the list opens the agent-status board (and
// loads it); tab again advances the cycle to the Workflows board; esc from the
// board returns to the Vendors list (and reloads).
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
	// tab advances the cycle to the Workflows board (loads it + bumps epoch).
	mw, cmd := press(t, m, "tab")
	if mw.screen != screenWorkflows || cmd == nil {
		t.Fatalf("tab from board: screen=%d cmd=%v, want screenWorkflows + load cmd", mw.screen, cmd)
	}
	if mw.workflowsEpoch != 1 {
		t.Fatalf("entering Workflows should bump workflowsEpoch to 1, got %d", mw.workflowsEpoch)
	}
	// Close the 3-way cycle: tab from Workflows returns to the Vendors list (+reload).
	mback, cmd := press(t, mw, "tab")
	if mback.screen != screenList || cmd == nil {
		t.Fatalf("tab from Workflows: screen=%d cmd=%v, want screenList + reload cmd", mback.screen, cmd)
	}
	// esc from Workflows also returns to the Vendors list.
	if mesc, _ := press(t, mw, "esc"); mesc.screen != screenList {
		t.Fatalf("esc from Workflows: screen=%d, want screenList", mesc.screen)
	}
	// esc from the board returns to the Vendors list (and reloads).
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

// boardModel enters the agent-status board with the given teammates + jobs
// already loaded (screen=screenSpawn, boardEpoch=1, loading=false).
func boardModel(t *testing.T, tms []teardown.Teammate, jobs []subagent.Result) Model {
	t.Helper()
	m := withVendors(t, userops.VendorView{Name: "glm"})
	m, _ = press(t, m, "tab") // enter board (epoch 1, loading)
	// stamp the live boardEpoch so the gate accepts the refresh.
	m, _ = step(t, m, boardMsg{teammates: tms, jobs: jobs, epoch: m.boardEpoch})
	return m
}

// TestBoardRendersTablesAndColumns: the board renders both the teammate table
// (with STATUS + HIDDEN columns) and the subagent-job table (status columns
// only), plus the new footer — and NEVER the job's answer text.
func TestBoardRendersTablesAndColumns(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "t1", Vendor: "glm", Model: "glm-4.6", PaneID: "%3", PID: 42, Status: "ok"},
			{Name: "bob", Team: "t2", Vendor: "kimi", Model: "kimi-k2", PaneID: "%4", PID: 43, Status: "error", Hidden: true},
		},
		[]subagent.Result{{
			JobID: "abcdef0123456789", Vendor: "glm", Model: "glm-4.6",
			Status: "running", StartedAt: "2026-05-26T01:02:03Z", LeadSessionID: "session-one-abcdef",
			Result: "TOP-SECRET-ANSWER", // must never render
		}},
	)
	out := m.View()
	for _, want := range []string{
		"NAME", "STATUS", "HIDDEN", "Subagent Jobs", "JOB", "STARTED",
		"◆ session: (no session)", "alice", "bob", "yes",
		"◆ session: session-", "abcdef01", "h hide", "s show",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("board view missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "TOP-SECRET-ANSWER") {
		t.Errorf("board leaked the job answer text (Result.Result) into the table:\n%s", out)
	}
}

// TestBoardCursorClampAndKeys: ↑/↓ clamp the teammate cursor at both ends, and
// h/s issue a command for the selected row.
func TestBoardCursorClampAndKeys(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%3"},
		{Name: "bob", Team: "t2", PaneID: "%4"},
	}, nil)
	if m.spawnCursor != 0 {
		t.Fatalf("initial cursor = %d, want 0", m.spawnCursor)
	}
	m, _ = press(t, m, "up")
	if m.spawnCursor != 0 {
		t.Fatalf("up at top: cursor = %d, want 0 (clamped)", m.spawnCursor)
	}
	m, _ = press(t, m, "down")
	if m.spawnCursor != 1 {
		t.Fatalf("down: cursor = %d, want 1", m.spawnCursor)
	}
	m, _ = press(t, m, "down")
	if m.spawnCursor != 1 {
		t.Fatalf("down at bottom: cursor = %d, want 1 (clamped)", m.spawnCursor)
	}
	if _, cmd := press(t, m, "h"); cmd == nil {
		t.Fatal("h on a teammate row should issue a hide command")
	}
	if _, cmd := press(t, m, "s"); cmd == nil {
		t.Fatal("s on a teammate row should issue a show command")
	}
}

// TestBoardHideShowNoOpWhenEmpty: h/s are no-ops (no command, stay on the board)
// when there are no teammates to act on.
func TestBoardHideShowNoOpWhenEmpty(t *testing.T) {
	m := boardModel(t, nil, nil)
	mh, cmd := press(t, m, "h")
	if cmd != nil || mh.screen != screenSpawn {
		t.Fatalf("h with no teammates: cmd=%v screen=%d, want nil + screenSpawn", cmd, mh.screen)
	}
	if _, cmd := press(t, m, "s"); cmd != nil {
		t.Fatal("s with no teammates should be a no-op (nil cmd)")
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

// TestBoardCursorClampsOnReload: when a refresh returns fewer rows than the
// cursor's position, the cursor clamps back into range.
func TestBoardCursorClampsOnReload(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{{Name: "a"}, {Name: "b"}, {Name: "c"}}, nil)
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	if m.spawnCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.spawnCursor)
	}
	m, _ = step(t, m, boardMsg{teammates: []teardown.Teammate{{Name: "a"}}, epoch: m.boardEpoch})
	if m.spawnCursor != 0 {
		t.Fatalf("after shrinking to 1 row: cursor = %d, want 0 (clamped)", m.spawnCursor)
	}
	m, _ = step(t, m, boardMsg{teammates: nil, epoch: m.boardEpoch})
	if m.spawnCursor != 0 {
		t.Fatalf("after empty reload: cursor = %d, want 0", m.spawnCursor)
	}
}

// TestBoardGroupsByTeam: interleaved sessions and teams on input are rendered
// as session → team → members; the cursor stays a flat teammate
// index that lands only on members, and h/s act on the selected member.
func TestBoardGroupsByTeam(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-bbbbbbbb", SpawnTime: 200},
		{Name: "bob", Team: "t2", PaneID: "%2", LeadSessionID: "sess-aaaaaaaa", SpawnTime: 100},
		{Name: "carol", Team: "t1", PaneID: "%3", LeadSessionID: "sess-bbbbbbbb", SpawnTime: 210},
		{Name: "dave", Team: "t3", PaneID: "%4"},
	}, nil)
	// Grouped by session earliest SpawnTime: sess-a{bob}, sess-b{alice,carol},
	// then no-session{dave} last.
	wantOrder := []string{"bob", "alice", "carol", "dave"}
	for i, want := range wantOrder {
		if m.teammates[i].Name != want {
			t.Fatalf("teammates[%d] = %q, want %q; all=%+v", i, m.teammates[i].Name, want, m.teammates)
		}
	}
	out := m.View()
	if strings.Count(out, "◆ session: sess-aaa…") != 1 ||
		strings.Count(out, "◆ session: sess-bbb…") != 1 ||
		strings.Count(out, "◆ session: (no session)") != 1 {
		t.Fatalf("want exactly one header per session:\n%s", out)
	}
	if strings.Count(out, "▸ team: t1") != 1 ||
		strings.Count(out, "▸ team: t2") != 1 ||
		strings.Count(out, "▸ team: t3") != 1 {
		t.Fatalf("want exactly one header per team:\n%s", out)
	}
	if !strings.Contains(out, "  ▸ team: t1") {
		t.Errorf("team header should be indented under its session:\n%s", out)
	}
	for _, name := range wantOrder {
		if !strings.Contains(out, name) {
			t.Errorf("grouped board missing member %q:\n%s", name, out)
		}
	}
	// Cursor walks members only (4 of them); down twice lands on carol (the 3rd).
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down")
	if m.spawnCursor != 2 || m.teammates[m.spawnCursor].Name != "carol" {
		t.Fatalf("cursor=%d member=%q, want 2/carol", m.spawnCursor, m.teammates[m.spawnCursor].Name)
	}
	m, _ = press(t, m, "down")
	m, _ = press(t, m, "down") // clamp at the last member, never onto a header
	if m.spawnCursor != 3 {
		t.Fatalf("cursor should clamp at the last member, got %d", m.spawnCursor)
	}
	if _, cmd := press(t, m, "h"); cmd == nil {
		t.Fatal("h on a grouped member row should issue a hide command")
	}
}

func TestBoardGroupsJobsBySession(t *testing.T) {
	m := boardModel(t, nil, []subagent.Result{
		{JobID: "job-b-newer", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T03:00:00Z", LeadSessionID: "sess-bbbbbbbb"},
		{JobID: "job-none", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T01:00:00Z"},
		{JobID: "job-a", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T02:00:00Z", LeadSessionID: "sess-aaaaaaaa"},
		{JobID: "job-b-older", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T00:30:00Z", LeadSessionID: "sess-bbbbbbbb"},
	})
	out := m.View()
	firstB := strings.Index(out, "◆ session: sess-bbb…")
	firstA := strings.Index(out, "◆ session: sess-aaa…")
	firstNone := strings.Index(out, "◆ session: (no session)")
	if firstB < 0 || firstA < 0 || firstNone < 0 {
		t.Fatalf("missing session headers:\n%s", out)
	}
	// sess-b has the earliest job (00:30), then sess-a (02:00), then no-session last.
	if !(firstB < firstA && firstA < firstNone) {
		t.Fatalf("job session headers not ordered by earliest StartedAt with no-session last:\n%s", out)
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("job grouping must still avoid rendering job answers:\n%s", out)
	}
}

func TestBoardSessionHeadersUseResolvedTitles(t *testing.T) {
	m := boardModel(t,
		[]teardown.Teammate{
			{Name: "alice", Team: "t1", PaneID: "%1", LeadSessionID: "sess-aaaaaaaa"},
		},
		[]subagent.Result{{
			JobID: "job-a", Vendor: "glm", Status: "done", StartedAt: "2026-05-26T02:00:00Z",
			LeadSessionID: "sess-aaaaaaaa", Result: "TOP-SECRET",
		}},
	)
	m.sessionTitles = map[string]string{"sess-aaaaaaaa": "Readable Session Name"}

	out := m.View()
	if strings.Count(out, "◆ session: Readable Session Name (sess-aaa…)") != 2 {
		t.Fatalf("expected titled session header in teammate and job tables:\n%s", out)
	}
	if strings.Contains(out, "TOP-SECRET") {
		t.Fatalf("session title rendering must not leak job answers:\n%s", out)
	}
}

// TestBoardEnterOpensTeammateDetail: enter on a board member opens the
// full-field detail card showing UNtruncated vendor/model (the table clips them);
// esc returns to the board with the cursor preserved.
func TestBoardEnterOpensTeammateDetail(t *testing.T) {
	m := boardModel(t, []teardown.Teammate{
		{Name: "worker-1", Team: "alpha", Vendor: "xiaomimimo", Model: "mimo-v2-flash", PaneID: "%7", PID: 4242, Status: "ok"},
	}, nil)
	m, _ = press(t, m, "enter")
	if m.screen != screenTeammateDetail {
		t.Fatalf("enter on a member: screen=%d, want screenTeammateDetail", m.screen)
	}
	out := m.View()
	// "xiaomimimo" (10) and "mimo-v2-flash" (13) exceed the table's 9/16-wide
	// columns only after trunc; the detail card must render them in full.
	for _, want := range []string{"worker-1", "alpha", "xiaomimimo", "mimo-v2-flash", "%7", "4242"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail card missing %q:\n%s", want, out)
		}
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenSpawn {
		t.Fatalf("esc from detail: screen=%d, want screenSpawn", m.screen)
	}
}

// TestBoardEnterNoOpWhenEmpty: enter with no teammates stays on the board.
func TestBoardEnterNoOpWhenEmpty(t *testing.T) {
	m := boardModel(t, nil, nil)
	m, _ = press(t, m, "enter")
	if m.screen != screenSpawn {
		t.Fatalf("enter with no teammates: screen=%d, want screenSpawn (no-op)", m.screen)
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
		screenList, screenSpawn, screenWorkflows, screenPickTemplate,
		screenForm, screenModelPick, screenRemoveConfirm, screenResult, screenKeys,
		screenTeammateDetail, screenWorkflowDetail,
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
// Workflows board (screenWorkflows)
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

// TestPartition_RunTaggedJobNotOnSpawnBoard: a RunID-tagged job belongs to the
// Workflows board and must NOT appear in the agent-status job table — the spawn
// board groups/shows only RunID == "" jobs.
func TestPartition_RunTaggedJobNotOnSpawnBoard(t *testing.T) {
	tagged := subagent.Result{JobID: "tagged00", RunID: "run-1", Phase: "build",
		Vendor: "glm", Status: "running", StartedAt: "2026-05-01T00:00:00Z"}
	plain := subagent.Result{JobID: "plain000", Vendor: "kimi", Status: "done",
		StartedAt: "2026-05-01T00:00:00Z"}
	m := boardModel(t, nil, []subagent.Result{tagged, plain})
	out := m.viewJobTable()
	if strings.Contains(out, "tagged00") {
		t.Errorf("RunID-tagged job leaked into the spawn job table:\n%s", out)
	}
	if !strings.Contains(out, "plain000") {
		t.Errorf("ungrouped job should appear in the spawn job table:\n%s", out)
	}
	// And groupedJobsBySession itself only sees the filtered slice when fed it.
	buckets := groupedJobsBySession([]subagent.Result{plain})
	for _, bk := range buckets {
		for _, j := range bk.jobs {
			if j.RunID != "" {
				t.Errorf("groupedJobsBySession bucket carried a RunID-tagged job: %+v", j)
			}
		}
	}
}

// TestWorkflowsMsg_StaleEpochDropped: a workflowsMsg whose epoch != the model's
// current workflowsEpoch (a prior visit's in-flight refresh) is dropped — the
// fresh visit's loading=true / empty state survives.
func TestWorkflowsMsg_StaleEpochDropped(t *testing.T) {
	// Enter the board, then advance to Workflows (workflowsEpoch -> 1, loading).
	m := boardModel(t, nil, nil)
	m, _ = press(t, m, "tab") // -> Workflows, epoch 1
	if m.screen != screenWorkflows || m.workflowsEpoch != 1 || !m.loading {
		t.Fatalf("setup: screen=%d epoch=%d loading=%v, want Workflows + epoch 1 + loading",
			m.screen, m.workflowsEpoch, m.loading)
	}
	stale := workflowsMsg{
		jobs:  []subagent.Result{{RunID: "run-z", Phase: "p", JobID: "leak0000"}},
		runs:  []subagent.WorkflowRun{{RunID: "run-z"}},
		epoch: 0, // from before the bump; must be dropped
	}
	m, _ = step(t, m, stale)
	if !m.loading {
		t.Fatal("stale-epoch workflowsMsg flipped m.loading to false")
	}
	if len(m.workflowJobs) != 0 || len(m.workflowRuns) != 0 {
		t.Fatalf("stale-epoch workflowsMsg leaked data: jobs=%+v runs=%+v",
			m.workflowJobs, m.workflowRuns)
	}
	// A matching-epoch msg DOES land (sanity).
	fresh := workflowsMsg{
		jobs:  []subagent.Result{{RunID: "run-z", Phase: "p", JobID: "ok000000", Label: "agent-a"}},
		runs:  []subagent.WorkflowRun{{RunID: "run-z", Name: "z", StartedAt: "2026-05-01T00:00:00Z"}},
		epoch: m.workflowsEpoch,
	}
	m, _ = step(t, m, fresh)
	if m.loading || len(m.workflowJobs) != 1 || len(m.workflowRuns) != 1 {
		t.Fatalf("matching workflowsMsg dropped: loading=%v jobs=%d runs=%d",
			m.loading, len(m.workflowJobs), len(m.workflowRuns))
	}
	if out := m.viewWorkflows(); !strings.Contains(out, "agent-a") || !strings.Contains(out, "Workflows") {
		t.Fatalf("workflows view missing expected content:\n%s", out)
	}
}

// TestWorkflowsTickReschedulesOnBoardStopsElsewhere: a current-epoch Workflows
// tick reschedules; a stale-epoch tick, or any tick once off the Workflows board,
// stops the chain (mirror of TestBoardTickReschedulesOnBoardStopsElsewhere).
func TestWorkflowsTickReschedulesOnBoardStopsElsewhere(t *testing.T) {
	m := boardModel(t, nil, nil)
	m, _ = press(t, m, "tab") // -> Workflows, workflowsEpoch 1
	if m.screen != screenWorkflows || m.workflowsEpoch != 1 {
		t.Fatalf("setup: screen=%d epoch=%d, want Workflows + epoch 1", m.screen, m.workflowsEpoch)
	}
	if _, cmd := step(t, m, workflowsTickMsg{epoch: m.workflowsEpoch}); cmd == nil {
		t.Fatal("current-epoch Workflows tick should reschedule (non-nil cmd)")
	}
	if _, cmd := step(t, m, workflowsTickMsg{epoch: 0}); cmd != nil {
		t.Fatal("stale-epoch Workflows tick should not reschedule")
	}
	mlist, _ := press(t, m, "tab") // Workflows → Vendors list
	if mlist.screen != screenList {
		t.Fatalf("tab should return to the list, screen = %d", mlist.screen)
	}
	if _, cmd := step(t, mlist, workflowsTickMsg{epoch: mlist.workflowsEpoch}); cmd != nil {
		t.Fatal("a Workflows tick fired while off the board should stop the chain (nil cmd)")
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
