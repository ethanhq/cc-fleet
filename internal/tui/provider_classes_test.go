package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// The run-permission value colors mirror Claude Code's own permission-mode indicator
// on both backgrounds; default/off stay in the plain body color.
func TestPermModeStyleMapping(t *testing.T) {
	want := map[string]lipgloss.AdaptiveColor{
		permmode.Plan:              {Light: "23", Dark: "66"},
		permmode.AcceptEdits:       {Light: "93", Dark: "141"},
		permmode.Auto:              {Light: "94", Dark: "214"},
		permmode.BypassPermissions: {Light: "160", Dark: "203"},
	}
	for mode, c := range want {
		if got := permModeStyle(mode).GetForeground(); got != c {
			t.Errorf("permModeStyle(%s) = %v, want %v", mode, got, c)
		}
	}
	for _, plain := range []string{"off", permmode.Default, ""} {
		if got := permModeStyle(plain).GetForeground(); got != contentStyle.GetForeground() {
			t.Errorf("permModeStyle(%q) = %v, want the plain body color", plain, got)
		}
	}
}

// Flat-picker item indices (cursor walks the selectable rows across all groups).
func firstOpenAIIdx() int { return len(Templates) + 1 } // after the Anthropic seeds + their Custom
func codexIdx() int       { return len(Templates) + 1 + len(OAITemplates) + 1 }

func pressN(t *testing.T, m Model, key string, n int) Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m, _ = press(t, m, key)
	}
	return m
}

// The grouped picker is one screen: enter (or →) on a row opens its class's form,
// with no intermediate class step.
func TestAddPicker_RightKeyOpensForm(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // +Add -> grouped picker (cursor 0 = DeepSeek)
	if m.screen != screenPickTemplate {
		t.Fatalf("screen = %d, want screenPickTemplate", m.screen)
	}
	m, _ = press(t, m, "right") // → descends straight to the form (no class step)
	if m.screen != screenForm || m.addProtocol != "" {
		t.Fatalf("right on a row: screen=%d addProtocol=%q", m.screen, m.addProtocol)
	}
	if m.form.value("name") != Templates[0].Name {
		t.Fatalf("form name = %q, want %q", m.form.value("name"), Templates[0].Name)
	}
}

// Selecting an OpenAI row lands on the OpenAI add form with the protocol carried
// and upstream_url prefilled.
func TestAddPicker_OpenAIRowCarriesProtocol(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	m = pressN(t, m, "down", firstOpenAIIdx()) // to the first OpenAI row (Responses)
	m, _ = press(t, m, "enter")
	if m.screen != screenForm || m.addProtocol != config.ProtocolOpenAIResponses {
		t.Fatalf("screen=%d addProtocol=%q", m.screen, m.addProtocol)
	}
	if got := m.form.value("upstream_url"); got != "https://api.openai.com/v1" {
		t.Fatalf("upstream_url prefill = %q", got)
	}
}

func TestAddPicker_OpenAISubmitValidatesThenDispatches(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	m = pressN(t, m, "down", firstOpenAIIdx())
	m, _ = press(t, m, "enter") // OpenAI Responses form (key/model blank)

	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")
	if cmd != nil || m.form.err == "" {
		t.Fatalf("incomplete OpenAI submit should block: cmd=%v err=%q", cmd, m.form.err)
	}
	m.form.setValue("api_key", "sk-openai-test")
	m.form.setValue("default_model", "gpt-x")
	m, cmd = press(t, m, "enter")
	if cmd == nil || !m.loading {
		t.Fatalf("complete OpenAI submit should dispatch: cmd=%v loading=%v", cmd, m.loading)
	}
}

// The codex row opens the add form; submitting it with no usable source commits the
// row, and once the add lands (opDoneMsg) it routes to consent → device login; esc
// rolls the row back and returns to the list.
func TestAddPicker_CodexRowNoSourceGoesToConsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())            // no ~/.codex
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no own login
	m := NewModel()
	// The blank config dir also reads as a fresh install with agent-teams
	// unconfigured, so NewModel opens on the first-run setup nudge; this test
	// exercises the picker, so start from the provider list.
	m.screen, m.loading = screenList, true
	m, _ = press(t, m, "enter")
	m = pressN(t, m, "down", codexIdx()) // to the codex row
	m, _ = press(t, m, "enter")
	if m.screen != screenForm || m.addProtocol != config.ProtocolCodexOAuth {
		t.Fatalf("codex row should open the add form: screen=%d proto=%q", m.screen, m.addProtocol)
	}
	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")               // submit -> commit the row first
	if cmd == nil || m.codexCommittedName == "" {
		t.Fatalf("no-source codex submit must commit first: cmd=%v committed=%q", cmd, m.codexCommittedName)
	}
	// The committing stage owns the screen for the async add: the form can no longer
	// be escaped or repopulated, so the login modal's underlay stays the submitted form.
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthCommitting {
		t.Fatalf("submit must park on committing: screen=%d stage=%d", m.screen, m.codexAuthStage)
	}
	m, cmd = press(t, m, "esc") // trapped — no abandon, no form
	if cmd != nil || m.screen != screenCodexAuth || m.codexCommittedName == "" {
		t.Fatalf("keys during committing must be swallowed: cmd=%v screen=%d", cmd, m.screen)
	}
	m, _ = step(t, m, opDoneMsg{verb: "add", name: m.codexCommittedName}) // add landed -> consent
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthConsent {
		t.Fatalf("after add, codex must route to consent: screen=%d stage=%d", m.screen, m.codexAuthStage)
	}
	m, cmd = press(t, m, "enter") // accept consent -> device stage
	if m.codexAuthStage != codexAuthDevice || cmd == nil {
		t.Fatalf("consent accept: stage=%d cmd=%v", m.codexAuthStage, cmd)
	}
	m, cmd = press(t, m, "esc") // abandon -> rollback the committed row
	if cmd == nil || m.codexCommittedName != "" {
		t.Fatalf("esc must roll back the committed row: cmd=%v committed=%q", cmd, m.codexCommittedName)
	}
	m, _ = step(t, m, codexRollbackDoneMsg{epoch: m.codexAuthEpoch})
	if m.screen != screenList {
		t.Fatalf("after rollback: screen=%d, want screenList", m.screen)
	}
}

// The first codex (no existing codex holds the default credential) claims the
// default, cli-ride-capable credential: it commits the row keyed on the legacy
// sentinel secret_ref before routing to its login.
func TestFirstCodexClaimsDefaultCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())            // no ~/.codex
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no own login
	m := withProviders(t)                    // no providers yet
	if m.codexDefaultTaken() {
		t.Fatal("with no codex, the default credential must be free")
	}
	mm, _ := m.enterCodexFlow()
	m = mm.(Model)
	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")               // no source -> commit the row first
	if cmd == nil || m.codexCommittedName != "codex" {
		t.Fatalf("first codex must commit the row, got committed=%q cmd=%v", m.codexCommittedName, cmd)
	}
	if m.codexAddRef != codexproxy.SecretRef {
		t.Fatalf("codexAddRef = %q, want the sentinel", m.codexAddRef)
	}
}

// The reserved native-leaf name is rejected by the central submitAdd gate for
// every add form — before the codex flow could start a device login.
func TestSubmitAdd_ReservedNativeNameRejected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withProviders(t)
	mm, _ := m.enterCodexFlow()
	m = mm.(Model)
	m.form.setValue("name", "claude")
	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")
	if m.screen != screenForm || !strings.Contains(m.form.err, "reserved for the native leaf") {
		t.Fatalf("reserved name must fail on the form: screen=%d err=%q", m.screen, m.form.err)
	}
	if m.codexPendingAdd != nil || cmd != nil {
		t.Fatal("reserved name must not stash a request or start a login")
	}
}

// An invalid codex provider name is rejected at submit BEFORE any device login —
// no auth screen, no credential file written, just a form error.
func TestCodexInvalidNameRejectedBeforeLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m := withProviders(t)
	mm, _ := m.enterCodexFlow()
	m = mm.(Model)
	m.form.setValue("name", "../escape")
	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")
	if m.screen != screenForm || m.form.err == "" {
		t.Fatalf("invalid name must fail on the form: screen=%d err=%q", m.screen, m.form.err)
	}
	if m.codexPendingAdd != nil || cmd != nil {
		t.Fatal("invalid name must not stash a request or start a login")
	}
}

// A second codex (one already holds the default credential) gets its own named
// credential keyed on the unique provider name, and submitting routes to its own
// device login.
func TestSecondCodexGetsOwnCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	existing := userops.ProviderView{
		Name: "codex", BaseURL: "http://127.0.0.1:17222/", ModelsEndpoint: "http://127.0.0.1:17222/v1/models",
		DefaultModel: "gpt-5.5", SecretBackend: config.CodexOAuthBackend, SecretRef: config.CodexOAuthBackend,
		Protocol: config.ProtocolCodexOAuth, Enabled: true,
	}
	m := withProviders(t, existing)
	if !m.codexDefaultTaken() {
		t.Fatal("an existing default-credential codex must mark the default taken")
	}
	mm, _ := m.enterCodexFlow()
	m = mm.(Model)
	if m.screen != screenForm {
		t.Fatalf("enterCodexFlow should open the form: screen=%d", m.screen)
	}
	m = pressN(t, m, "down", len(m.form.fields)) // jump to submit
	m, cmd := press(t, m, "enter")               // commits the row first
	if cmd == nil || m.codexCommittedName != "codex-2" {
		t.Fatalf("second codex must commit a named-credential row, got committed=%q cmd=%v", m.codexCommittedName, cmd)
	}
	if m.codexAddRef != "codex-2" {
		t.Fatalf("codexAddRef = %q, want codex-2", m.codexAddRef)
	}
	m, _ = step(t, m, opDoneMsg{verb: "add", name: "codex-2"}) // add landed -> its own login
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthConsent {
		t.Fatalf("second codex add must route to its own login: screen=%d stage=%d", m.screen, m.codexAuthStage)
	}

	// Naming the second codex with the sentinel would collapse onto the default
	// credential: rejected on the form before any commit.
	mm2, _ := withProviders(t, existing).enterCodexFlow()
	m2 := mm2.(Model)
	m2.form.setValue("name", codexproxy.SecretRef)
	m2 = pressN(t, m2, "down", len(m2.form.fields))
	m2, cmd2 := press(t, m2, "enter")
	if m2.screen != screenForm || m2.form.err == "" || m2.codexCommittedName != "" || cmd2 != nil {
		t.Fatalf("sentinel-named second codex must be rejected on the form: screen=%d err=%q", m2.screen, m2.form.err)
	}
}

// Abandoning a codex device login cancels the session, bumps the epoch, and rolls the
// committed row back — so a stale in-flight poll result can neither show success nor
// persist a login behind the cancelled add.
func TestCodexAbandonCancelsAndDropsStalePoll(t *testing.T) {
	m := NewModel()
	m.screen = screenCodexAuth
	m.codexAuthStage = codexAuthDevice
	m.codexAuthEpoch = 3
	m.codexAuth = &codexproxy.LoginSession{}
	m.codexCommittedName = "codex"
	ctx, cancel := context.WithCancel(context.Background())
	m.codexAuthCtx, m.codexAuthCancel = ctx, cancel

	mm, cmd := m.codexAbandonLogin()
	m = mm.(Model)
	if cmd == nil || m.codexAuthEpoch != 4 || m.codexCommittedName != "" || m.codexAuth != nil {
		t.Fatalf("abandon must bump epoch, clear state, and roll back: epoch=%d committed=%q auth=%v cmd=%v",
			m.codexAuthEpoch, m.codexCommittedName, m.codexAuth, cmd)
	}
	if ctx.Err() == nil {
		t.Fatal("abandon must cancel the session context so an in-flight poll cannot persist a token")
	}
	// The in-flight poll's stale-epoch result must be dropped — no success outcome.
	if m2, _ := step(t, m, codexAuthPollMsg{epoch: 3, done: true}); m2.screen != screenCodexAuth || m2.confirm != nil {
		t.Fatal("a stale-epoch poll-done after abandon must be dropped, not shown as success")
	}
}

// When a codex subscription is detected (cli-ride), the add routes to a source choice:
// reuse commits as-is; separate login commits the row first (marking it for the login);
// esc returns to the form.
func TestCodexChooseSourceBranches(t *testing.T) {
	base := func() Model {
		m := NewModel()
		m.screen = screenCodexAuth
		m.codexAuthStage = codexAuthChooseSource
		req := userops.AddRequest{Name: "codex", SecretRef: codexproxy.SecretRef, Protocol: config.ProtocolCodexOAuth}
		m.codexPendingAdd = &req
		m.codexAddRef = codexproxy.SecretRef
		return m
	}
	// enter on the default cursor (0 = reuse) commits with no follow-up login.
	if m, cmd := press(t, base(), "enter"); cmd == nil || !m.loading || m.codexPendingAdd != nil || m.codexCommittedName != "" || m.codexAuthStage != codexAuthCommitting {
		t.Fatalf("reuse must commit with no follow-up login and park on committing: cmd=%v loading=%v pending=%v committed=%q stage=%d",
			cmd, m.loading, m.codexPendingAdd, m.codexCommittedName, m.codexAuthStage)
	}
	// ↓ then enter (cursor 1 = separate login) commits the row first and marks it.
	m := base()
	m, _ = press(t, m, "down")
	if m.codexSourceCursor != 1 {
		t.Fatalf("down should move the source cursor to 1, got %d", m.codexSourceCursor)
	}
	m, _ = press(t, m, "down") // clamped at the last option
	if m.codexSourceCursor != 1 {
		t.Fatalf("down past the end should clamp, got %d", m.codexSourceCursor)
	}
	m, cmd := press(t, m, "enter")
	if cmd == nil || m.codexPendingAdd != nil || m.codexCommittedName != "codex" || m.codexAuthStage != codexAuthCommitting {
		t.Fatalf("separate login must commit the row first, mark it, and park on committing: cmd=%v pending=%v committed=%q stage=%d",
			cmd, m.codexPendingAdd, m.codexCommittedName, m.codexAuthStage)
	}
	if m, _ := press(t, base(), "esc"); m.screen != screenForm || m.codexPendingAdd != nil {
		t.Fatalf("esc must return to the form and drop the pending add: screen=%d", m.screen)
	}
}

// A failed add during the committing stage falls through to the failure modal exactly
// like any other failed op — no consent detour, marker cleared.
func TestCodexCommittingFailureRoutesToResult(t *testing.T) {
	m := NewModel()
	m.screen = screenCodexAuth
	m.codexAuthStage = codexAuthCommitting
	m.codexCommittedName = "codex"
	m.loading = true
	m, _ = step(t, m, opDoneMsg{verb: "add", name: "codex", err: errors.New("boom")})
	if m.screen != screenList || m.confirm == nil || !m.confirm.resultErr || m.codexCommittedName != "" {
		t.Fatalf("failed committing add: screen=%d confirmOpen=%v committed=%q, want list+red modal+cleared",
			m.screen, m.confirm != nil, m.codexCommittedName)
	}
}

// An opDoneMsg from an earlier, unrelated op (a stale dispatch) arriving during a marked
// codex commit is dropped outright — the user stays parked on committing (so a second
// codex commit can never overwrite the marker) and only the committed add's own result
// drives the flow.
func TestCodexCommittingIgnoresUnrelatedOpDone(t *testing.T) {
	m := NewModel()
	m.screen = screenCodexAuth
	m.codexAuthStage = codexAuthCommitting
	m.codexCommittedName = "codex"
	m.loading = true
	m, _ = step(t, m, opDoneMsg{verb: "add", name: "other"})
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthCommitting || !m.loading {
		t.Fatalf("an unrelated result must be dropped, not leave committing: screen=%d stage=%d loading=%v",
			m.screen, m.codexAuthStage, m.loading)
	}
	if m.codexCommittedName != "codex" {
		t.Fatalf("an unrelated result cleared the marker: %q", m.codexCommittedName)
	}
	m, _ = step(t, m, opDoneMsg{verb: "add", name: "codex"})
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthConsent {
		t.Fatalf("the committed add's result must route to consent: screen=%d stage=%d", m.screen, m.codexAuthStage)
	}
	// The guard covers the whole marked flow: a stale unrelated result arriving on the
	// consent (or device) stage is dropped the same way.
	m, _ = step(t, m, opDoneMsg{verb: "add", name: "other"})
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthConsent || m.codexCommittedName != "codex" {
		t.Fatalf("an unrelated result on consent must be dropped: screen=%d stage=%d marker=%q",
			m.screen, m.codexAuthStage, m.codexCommittedName)
	}
}

// The codex login renders as a centered modal over the form base: the device stage shows
// the verify URL + user code inside the box, the consent stage the risk notice, and the
// base's footer no longer advertises the form's own keys.
func TestCodexAuthModalContent(t *testing.T) {
	m := NewModel()
	m.screen = screenCodexAuth
	m.form = newAddForm(Templates[0])
	m.codexAuthStage = codexAuthDevice
	m.codexAuth = &codexproxy.LoginSession{VerifyURL: "https://auth.openai.com/codex/device", UserCode: "ABCD-1234"}
	out := m.View()
	for _, want := range []string{"https://auth.openai.com/codex/device", "ABCD-1234", "waiting for authorization…"} {
		if !strings.Contains(out, want) {
			t.Fatalf("device modal missing %q", want)
		}
	}
	if strings.Contains(out, "enter on [Add] submits") {
		t.Fatal("the form footer must be blanked under the login modal")
	}

	m.codexAuthStage = codexAuthConsent
	m.codexAuth = nil
	out = m.View()
	if !strings.Contains(out, "device-code login") {
		t.Fatal("consent modal missing the risk/consent hint")
	}

	m.codexAuthStage = codexAuthCommitting
	if out = m.View(); !strings.Contains(out, "adding provider…") {
		t.Fatal("committing modal missing its wait line")
	}
}

// The read-only preview card and the edit form keep the same Note→Config gap, so
// entering edit does not shift the Config block down.
func TestPreviewMatchesEditNoteGap(t *testing.T) {
	v := userops.ProviderView{Name: "glm", DefaultModel: "glm-4.5-air", SecretBackend: "file"}
	gap := func(lines []string) int {
		note, cfg := -1, -1
		for i, l := range lines {
			if strings.Contains(l, "Note") && note < 0 {
				note = i
			}
			if strings.Contains(l, "Config") {
				cfg = i
			}
		}
		return cfg - note
	}
	const w = 60
	if p, e := gap(providerDetailLines(v, w, "")), gap(newEditForm(v).viewLines(w)); p != e {
		t.Fatalf("Note→Config gap differs (Config jumps entering edit): preview=%d edit=%d", p, e)
	}
}

// A codexAuthBegunMsg from a prior login attempt (esc then re-enter starts a new
// epoch) is dropped, never installing a stale session or starting a poll.
func TestCodexAuth_StaleEpochBegunDropped(t *testing.T) {
	m := NewModel()
	m.screen = screenCodexAuth
	m.codexAuthStage = codexAuthDevice
	m.codexAuthEpoch = 2 // the current attempt
	nm, cmd := step(t, m, codexAuthBegunMsg{epoch: 1})
	if cmd != nil {
		t.Fatal("a begin from a stale epoch must be dropped (no poll scheduled)")
	}
	if nm.codexAuth != nil {
		t.Fatalf("stale begin must not install a session: %v", nm.codexAuth)
	}
}

// The codex form's source note renders inside the Note section (after the "Note"
// header, before "Config"), wrapped to the pane — never as a banner above it.
func TestCodexFormNoteInNoteSection(t *testing.T) {
	f := newCodexAddForm("codex", "gpt-5.5", "reuses the codex CLI login (account …abc) — no key needed")
	lines := f.viewLines(48)
	noteHdr, cfgHdr, body := -1, -1, -1
	for i, l := range lines {
		switch {
		case strings.Contains(l, "Note"):
			noteHdr = i
		case strings.Contains(l, "Config"):
			cfgHdr = i
		case strings.Contains(l, "reuses the codex CLI login"):
			body = i
		}
	}
	if noteHdr < 0 || cfgHdr < 0 || body < 0 {
		t.Fatalf("missing section/body: note=%d config=%d body=%d", noteHdr, cfgHdr, body)
	}
	if !(noteHdr < body && body < cfgHdr) {
		t.Fatalf("note body must sit between Note and Config: note=%d body=%d config=%d", noteHdr, body, cfgHdr)
	}
}

// On the codex add form, enter on the default-model field opens the model picker
// seeded with the static codex model list (no live endpoint to probe at add time).
func TestCodexForm_DefaultOpensStaticModelPicker(t *testing.T) {
	m := NewModel()
	m.form = newCodexAddForm("codex", "gpt-5.5", "")
	m.formMode = modeAdd
	m.addProtocol = config.ProtocolCodexOAuth
	m.screen = screenForm

	m, _ = press(t, m, "down") // name -> default_model
	if m.form.focusedKey() != "default_model" {
		t.Fatalf("focus = %q, want default_model", m.form.focusedKey())
	}
	m, cmd := press(t, m, "enter")
	if m.screen != screenModelPick {
		t.Fatalf("screen = %d, want screenModelPick", m.screen)
	}
	if cmd != nil {
		t.Fatal("static picker needs no fetch command")
	}
	if len(m.modelList) == 0 || m.modelList[0].ID != "gpt-5.5" {
		t.Fatalf("model list = %v, want the static codex list", m.modelList)
	}
}

func focusEditDefault(t *testing.T, m Model) Model {
	t.Helper()
	for i := 0; i < len(m.form.fields); i++ {
		if m.form.focusedKey() == "default_model" {
			return m
		}
		m, _ = press(t, m, "down")
	}
	t.Fatalf("could not focus default_model (fields: %d)", len(m.form.fields))
	return m
}

// The edit-form model picker seeds from the static codex list for a codex
// provider, and fetches the real endpoint for a non-codex one — driven by the
// edited provider's class (so a prior codex flow can't leak into a deepseek edit).
func TestEditForm_ModelPickerSourceByClass(t *testing.T) {
	deepseek := userops.ProviderView{
		Name: "deepseek", BaseURL: "https://api.deepseek.com/anthropic",
		ModelsEndpoint: "https://api.deepseek.com/v1/models", DefaultModel: "x",
		SecretBackend: "file", Protocol: "", Enabled: true,
	}
	codex := userops.ProviderView{
		Name: "codex", BaseURL: "http://127.0.0.1:17222/", ModelsEndpoint: "http://127.0.0.1:17222/v1/models",
		DefaultModel: "gpt-5.5", SecretBackend: config.CodexOAuthBackend, Protocol: config.ProtocolCodexOAuth, Enabled: true,
	}
	m := withProviders(t, deepseek, codex) // class-sorted: deepseek (0), codex (1)

	// editing codex -> static codex list, and no key-manager row.
	mc := m
	mc.screen = screenList
	mc.providerCursor = 1
	mc, _ = press(t, mc, "enter")
	if mc.addProtocol != config.ProtocolCodexOAuth {
		t.Fatalf("edit codex addProtocol = %q", mc.addProtocol)
	}
	for _, fld := range mc.form.fields {
		if fld.key == "manage_keys" {
			t.Fatal("codex edit form must omit the key manager")
		}
	}
	mc = focusEditDefault(t, mc)
	mc, cmd := press(t, mc, "enter")
	if mc.screen != screenModelPick || cmd != nil || len(mc.modelList) == 0 || mc.modelList[0].ID != "gpt-5.5" {
		t.Fatalf("codex edit picker must be static: screen=%d cmd=%v list=%v", mc.screen, cmd, mc.modelList)
	}

	// editing deepseek -> fetch its own endpoint.
	md := m
	md.screen = screenList
	md.providerCursor = 0
	md, _ = press(t, md, "enter")
	if md.addProtocol != "" {
		t.Fatalf("edit deepseek addProtocol = %q, want empty", md.addProtocol)
	}
	md = focusEditDefault(t, md)
	md, cmd = press(t, md, "enter")
	if md.screen != screenModelPick || cmd == nil || !md.loading || md.modelList != nil {
		t.Fatalf("deepseek edit must fetch: cmd=%v loading=%v list=%v", cmd, md.loading, md.modelList)
	}
}

// The providers list is grouped by wire class: Anthropic-protocol, then
// OpenAI-protocol, then CLI auth (stable within a class).
func TestProvidersList_GroupedByClass(t *testing.T) {
	m := NewModel()
	in := []userops.ProviderView{
		{Name: "codex", Protocol: config.ProtocolCodexOAuth},
		{Name: "deepseek", Protocol: ""},
		{Name: "groq", Protocol: config.ProtocolOpenAIChat},
	}
	m, _ = step(t, m, providersMsg{providers: in})
	got := []string{m.providers[0].Name, m.providers[1].Name, m.providers[2].Name}
	want := []string{"deepseek", "groq", "codex"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("class order = %v, want %v", got, want)
		}
	}
}

// The edit form's editable endpoint follows the class: openai edits upstream_url
// (not the loopback base_url), codex edits neither, Anthropic-native edits base_url.
func TestEditForm_FieldsByClass(t *testing.T) {
	keys := func(f form) map[string]bool {
		s := map[string]bool{}
		for _, fld := range f.fields {
			s[fld.key] = true
		}
		return s
	}
	oai := keys(newEditForm(userops.ProviderView{Name: "openai", Protocol: config.ProtocolOpenAIResponses, UpstreamURL: "https://api.openai.com/v1", SecretBackend: "file"}))
	if !oai["upstream_url"] || oai["base_url"] || !oai["manage_keys"] {
		t.Fatalf("openai edit fields = %v (want upstream_url + keys, no base_url)", oai)
	}
	cdx := keys(newEditForm(userops.ProviderView{Name: "codex", Protocol: config.ProtocolCodexOAuth, SecretBackend: config.CodexOAuthBackend}))
	if cdx["base_url"] || cdx["models_endpoint"] || cdx["upstream_url"] || cdx["manage_keys"] {
		t.Fatalf("codex edit fields = %v (want only default_model + enabled)", cdx)
	}
	ant := keys(newEditForm(userops.ProviderView{Name: "deepseek", Protocol: "", SecretBackend: "file"}))
	if !ant["base_url"] || ant["upstream_url"] {
		t.Fatalf("anthropic edit fields = %v (want base_url, no upstream_url)", ant)
	}
}

func TestCodexSourceNote(t *testing.T) {
	if codexSourceNote(codexproxy.CredStatus{Active: "none"}) != "" {
		t.Fatal("no source must yield an empty note")
	}
	ride := codexSourceNote(codexproxy.CredStatus{Active: "cli-ride", Account: "…abc"})
	if !strings.Contains(ride, "codex CLI login") || !strings.Contains(ride, "…abc") {
		t.Fatalf("cli-ride note = %q", ride)
	}
	own := codexSourceNote(codexproxy.CredStatus{Active: "own", Account: "…xyz"})
	if !strings.Contains(own, "own codex login") {
		t.Fatalf("own note = %q", own)
	}
}

// A picker taller than the pane windows on the cursored row so the selection and
// its detail stay on screen, with ↑/↓ "more" markers; a pane tall enough to fit
// the whole list is returned unwindowed.
func TestAddPicker_WindowsTallListOnCursor(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // + Add -> grouped picker, cursor 0

	// Tall pane: the whole list fits, so it is returned unwindowed (no markers).
	m.height = 60
	joined := strings.Join(m.addPickerLines(m.tmplCursor), "\n")
	if !strings.Contains(joined, Templates[0].Label) || !strings.Contains(joined, "Codex") {
		t.Fatalf("tall pane should list every row:\n%s", joined)
	}
	if strings.Contains(joined, "↑") || strings.Contains(joined, "↓") {
		t.Fatalf("tall pane should not window (no markers):\n%s", joined)
	}

	// Short pane: the list overflows, so it windows on the cursor. At the top the
	// cursored row shows with a ↓ marker; at the bottom it scrolls into view with
	// a ↑ marker. The window never exceeds the pane height.
	m.height = 24 // boardBodyHeight 14, far shorter than the ~28-line list
	top := m.addPickerLines(m.tmplCursor)
	joined = strings.Join(top, "\n")
	if len(top) > m.boardBodyHeight() {
		t.Fatalf("top window = %d lines, want <= %d:\n%s", len(top), m.boardBodyHeight(), joined)
	}
	if !strings.Contains(joined, Templates[0].Label) || !strings.Contains(joined, "↓") {
		t.Fatalf("top window should show the cursored row and a ↓ marker:\n%s", joined)
	}

	// Mid-list: the cursor sits between hidden rows on both sides, so BOTH markers
	// render — the only case that exercises windowBounds' centered start branch.
	m = pressN(t, m, "down", 6) // to a middle Anthropic row
	mid := m.addPickerLines(m.tmplCursor)
	joined = strings.Join(mid, "\n")
	if len(mid) > m.boardBodyHeight() {
		t.Fatalf("mid window = %d lines, want <= %d:\n%s", len(mid), m.boardBodyHeight(), joined)
	}
	if !strings.Contains(joined, Templates[6].Label) ||
		!strings.Contains(joined, "↑") || !strings.Contains(joined, "↓") {
		t.Fatalf("mid window should show the cursored row and both ↑/↓ markers:\n%s", joined)
	}

	// Floor: at the minimum pane height the detail can't share the pane, so the
	// window is clamped to bodyH — the bound still holds and the cursored row stays.
	m.height = 12 // boardBodyHeight floors to 5
	floor := m.addPickerLines(m.tmplCursor)
	joined = strings.Join(floor, "\n")
	if len(floor) > m.boardBodyHeight() {
		t.Fatalf("floor window = %d lines, want <= %d:\n%s", len(floor), m.boardBodyHeight(), joined)
	}
	if !strings.Contains(joined, Templates[6].Label) {
		t.Fatalf("floor window should still show the cursored row:\n%s", joined)
	}
	m.height = 24 // restore for the bottom-edge check

	m = pressN(t, m, "down", codexIdx()-6) // continue to the last row (Codex)
	bottom := m.addPickerLines(m.tmplCursor)
	joined = strings.Join(bottom, "\n")
	if len(bottom) > m.boardBodyHeight() {
		t.Fatalf("bottom window = %d lines, want <= %d:\n%s", len(bottom), m.boardBodyHeight(), joined)
	}
	if !strings.Contains(joined, "Codex") || !strings.Contains(joined, "↑") {
		t.Fatalf("bottom window should show Codex and a ↑ marker:\n%s", joined)
	}
}

// Typing narrows the grouped picker to the matching rows, drops the groups that lose
// everything, and keeps the seed preview aligned with the highlighted survivor.
func TestAddPickerFilterNarrowsAndKeepsDetailAligned(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter") // + Add -> grouped picker
	m.width, m.height = 100, 40
	for _, r := range "moonshot" {
		m, _ = press(t, m, string(r))
	}
	if got := addItems(m.tmplFilter); len(got) != 1 || got[0].label != "Moonshot Kimi" {
		t.Fatalf("filter %q → %+v, want just Moonshot Kimi", m.tmplFilter, got)
	}
	if m.tmplCursor != 0 {
		t.Fatalf("tmplCursor = %d, want 0 (reset on filter)", m.tmplCursor)
	}
	out := m.View()
	if !strings.Contains(out, "Moonshot Kimi") || strings.Contains(out, "StepFun") {
		t.Fatalf("filtered view should show only the match:\n%s", out)
	}
	if strings.Contains(out, "OpenAI-protocol API") {
		t.Fatalf("a group with no surviving row should drop out:\n%s", out)
	}
	if !strings.Contains(out, "https://api.moonshot.cn/anthropic") {
		t.Fatalf("the seed preview must track the highlighted row:\n%s", out)
	}
	if want := fmt.Sprintf("(1/%d)", len(addItems(""))); !strings.Contains(out, want) {
		t.Fatalf("filter line should report %s:\n%s", want, out)
	}
	m, _ = press(t, m, "enter")
	if m.screen != screenForm || m.form.value("name") != "kimi" {
		t.Fatalf("enter should open the kimi add form; screen = %d, name = %q", m.screen, m.form.value("name"))
	}
}

// A query matches more than the visible label: the provider id the row seeds, its
// endpoint, and the protocol group it sits under.
func TestAddPickerFilterMatchesIdEndpointAndGroup(t *testing.T) {
	if got := addItems("zai"); len(got) != 1 || got[0].tIdx < 0 || Templates[got[0].tIdx].Name != "zai" {
		t.Fatalf("id query zai → %+v, want the zai preset", got)
	}
	if got := addItems("bigmodel"); len(got) != 1 || got[0].label != "Zhipu GLM" {
		t.Fatalf("endpoint query bigmodel → %+v, want Zhipu GLM", got)
	}
	got := addItems("openai") // group heading: the whole OpenAI-protocol class
	if len(got) != len(OAITemplates)+1 {
		t.Fatalf("group query openai → %d rows, want the whole OpenAI class", len(got))
	}
	for _, it := range got {
		if it.class != addCatOpenAI {
			t.Fatalf("group query openai returned a %v row: %+v", it.class, it)
		}
	}
}

// Nothing in the catalog matching is exactly what the manual rows are for, so they
// stand in rather than an empty list — and they are live, not decoration.
func TestAddPickerFilterNoMatchOffersCustom(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	m.width, m.height = 100, 40
	for _, r := range "mistral" {
		m, _ = press(t, m, string(r))
	}
	items := addItems(m.tmplFilter)
	if len(items) != 2 || items[0] != customAnthropicItem || items[1] != customOpenAIItem {
		t.Fatalf("no-match filter → %+v, want the two Custom rows", items)
	}
	out := m.View()
	if !strings.Contains(out, "no preset matches") {
		t.Fatalf("no-match view should say so:\n%s", out)
	}
	if want := fmt.Sprintf("(0/%d)", len(addItems(""))); !strings.Contains(out, want) {
		t.Fatalf("no-match count should be %s:\n%s", want, out)
	}
	m, _ = press(t, m, "enter")
	if m.screen != screenForm || m.form.value("name") != "" || m.form.value("base_url") != "" {
		t.Fatalf("enter on the fallback should open the blank custom form; screen = %d", m.screen)
	}
}

// Backspace widens the filter and stops at the start.
func TestAddPickerFilterBackspaceWidens(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	for _, r := range "kimi" {
		m, _ = press(t, m, string(r))
	}
	if got := len(addItems(m.tmplFilter)); got != 2 { // Moonshot Kimi + Kimi Code
		t.Fatalf("filter kimi → %d rows, want 2", got)
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyCtrlH}) // terminals that report Backspace as Ctrl-H
	for i := 0; i < 4; i++ {
		m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	}
	if m.tmplFilter != "" {
		t.Fatalf("tmplFilter = %q, want empty after backspacing past the start", m.tmplFilter)
	}
	if got, want := len(addItems(m.tmplFilter)), len(addItems("")); got != want {
		t.Fatalf("empty filter → %d rows, want the full %d", got, want)
	}
}

// Esc clears a live filter before it cancels, so the picker is never left narrowed.
func TestAddPickerEscClearsFilterThenCancels(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	for _, r := range "glm" {
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "esc")
	if m.tmplFilter != "" || m.screen != screenPickTemplate {
		t.Fatalf("first esc: filter = %q, screen = %d, want cleared and still on the picker", m.tmplFilter, m.screen)
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("second esc: screen = %d, want screenList", m.screen)
	}
}

// Printable j/k/q extend the filter rather than navigating, which is what makes the
// presets starting with those letters reachable by typing.
func TestAddPickerVimKeysTypeIntoFilter(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	m, _ = press(t, m, "k")
	if m.tmplFilter != "k" || m.tmplCursor != 0 {
		t.Fatalf("k should type, not move: filter = %q, cursor = %d", m.tmplFilter, m.tmplCursor)
	}
	m, _ = press(t, m, "esc") // clear and start over
	for _, r := range "qwen" {
		m, _ = press(t, m, string(r))
	}
	if m.screen != screenPickTemplate {
		t.Fatalf("q must not quit the picker; screen = %d", m.screen)
	}
	if got := addItems(m.tmplFilter); len(got) != 1 || got[0].label != "Qwen (Alibaba DashScope)" {
		t.Fatalf("typing qwen → %+v, want the Qwen preset", got)
	}
}

// The hub's read-only "+ Add provider…" preview always shows the whole catalog — the
// filter belongs to the picker the user is standing in, and picking a row leaves one
// behind.
func TestAddPickerHubPreviewIgnoresFilter(t *testing.T) {
	m := withProviders(t) // no providers: the cursor starts on the "+ Add" row
	m, _ = press(t, m, "enter")
	m.width, m.height = 100, 60
	for _, r := range "moonshot" {
		m, _ = press(t, m, string(r))
	}
	m, _ = press(t, m, "enter") // pick it: the add form keeps the filter behind
	m, _ = press(t, m, "esc")   // back to the hub, which reloads
	m, _ = step(t, m, providersMsg{})
	if m.screen != screenList {
		t.Fatalf("screen = %d, want screenList", m.screen)
	}
	if m.tmplFilter == "" {
		t.Fatal("this guards a live filter — it should still be set after picking")
	}
	out := m.View()
	if !strings.Contains(out, "StepFun") || !strings.Contains(out, "Codex") {
		t.Fatalf("the hub preview must show the whole catalog:\n%s", out)
	}
}

// The fallback's explanation rides on the filter block, which sits outside the row
// window — so a pane too short to hold the rows still says why they are the only ones
// left. Heights 12-16 are the band where the rows do get windowed.
func TestAddPickerFilterNoMatchExplainsOnShortPanes(t *testing.T) {
	for _, h := range []int{12, 14, 16, 24} {
		m := NewModel()
		m, _ = press(t, m, "enter")
		m.width, m.height = 100, h
		for _, r := range "mistral" {
			m, _ = press(t, m, string(r))
		}
		out := m.View()
		if !strings.Contains(out, "no preset matches") {
			t.Fatalf("height %d dropped the fallback explanation:\n%s", h, out)
		}
		if !strings.Contains(out, "Custom (fill everything manually)") {
			t.Fatalf("height %d dropped the fallback rows:\n%s", h, out)
		}
	}
}

// Enter acts on the highlighted row, so it must be on screen even when the pane is too
// tight for the markers and the seed preview — including the fallback's second row,
// where the window opens with an ↑ marker that would otherwise take the last line.
func TestAddPickerTightPaneKeepsHighlightVisible(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	m.width, m.height = 100, 12 // boardBodyHeight floors at 5; the 3-line no-match head leaves 2
	for _, r := range "mistral" {
		m, _ = press(t, m, string(r))
	}
	// Assert on the picker's own rows: the footer hint spells out "↑/↓", so a whole-view
	// substring check would pass on that alone.
	rows := strings.Join(m.addPickerLines(m.tmplCursor), "\n")
	if !strings.Contains(rows, "Custom (fill everything manually)") || !strings.Contains(rows, "↓ 1 more") {
		t.Fatalf("first row: want the highlight and a ↓ for the row it hides:\n%s", rows)
	}
	m, _ = press(t, m, "down") // to the OpenAI-compatible Custom row
	items := addItems(m.tmplFilter)
	if m.tmplCursor != 1 || items[1] != customOpenAIItem {
		t.Fatalf("cursor = %d over %+v, want the second fallback row", m.tmplCursor, items)
	}
	rows = strings.Join(m.addPickerLines(m.tmplCursor), "\n")
	if !strings.Contains(rows, "Custom OpenAI-compatible") {
		t.Fatalf("the highlighted row must stay visible on a tight pane:\n%s", rows)
	}
	if !strings.Contains(rows, "↑ 1 more") {
		t.Fatalf("second row: want a ↑ for the row it hides:\n%s", rows)
	}
}

// The window math has two invariants the picker cannot break at any size: Enter acts on
// the highlighted row, so that row is always rendered; and a row left off screen is
// always announced by a ↑/↓ marker. Swept over every pane height and cursor position
// because the head, the detail block and the markers all draw on one budget.
func TestAddPickerWindowInvariantsHoldAtEverySize(t *testing.T) {
	for _, q := range []string{"", "kimi", "mistral", "openai", "glm", "custom", "z"} {
		items := addItems(q)
		for h := 5; h <= 60; h++ {
			for cur := range items {
				m := NewModel()
				m.screen, m.loading = screenPickTemplate, false
				m.width, m.height = 100, h
				m.tmplFilter, m.tmplCursor = q, cur
				out := m.addPickerLines(cur)
				joined := strings.Join(out, "\n")
				if !strings.Contains(joined, "❯ "+items[cur].label) {
					t.Fatalf("h=%d q=%q cur=%d: the highlighted %q is off screen:\n%s", h, q, cur, items[cur].label, joined)
				}
				shown := 0
				for _, it := range items {
					for _, l := range out {
						if strings.HasSuffix(l, it.label) {
							shown++
							break
						}
					}
				}
				if shown < len(items) && !strings.Contains(joined, "↑ ") && !strings.Contains(joined, "↓ ") {
					t.Fatalf("h=%d q=%q cur=%d: %d of %d rows shown with no marker:\n%s", h, q, cur, shown, len(items), joined)
				}
			}
		}
	}
}

// A label is typed the way it is displayed, spaces included; and a bracketed paste
// arrives as one key message, so its newlines and control bytes are dropped rather
// than rendered through the pane border.
func TestAddPickerFilterAcceptsSpaceAndSanitizesPaste(t *testing.T) {
	m := NewModel()
	m, _ = press(t, m, "enter")
	for _, r := range "Kimi" {
		m, _ = press(t, m, string(r))
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeySpace})
	for _, r := range "Code" {
		m, _ = press(t, m, string(r))
	}
	if m.tmplFilter != "Kimi Code" {
		t.Fatalf("tmplFilter = %q, want the label as typed", m.tmplFilter)
	}
	if got := addItems(m.tmplFilter); len(got) != 1 || got[0].label != "Kimi Code" {
		t.Fatalf("typing the displayed label → %+v, want Kimi Code", got)
	}

	m2 := NewModel()
	m2, _ = press(t, m2, "enter")
	m2.width, m2.height = 100, 40
	m2, _ = step(t, m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ki\nmi\x1b[31m\x07")})
	if m2.tmplFilter != "kimi[31m" { // the ESC and BEL go, the letters it introduced stay
		t.Fatalf("tmplFilter = %q, want the printable remainder on one line", m2.tmplFilter)
	}
	if strings.ContainsAny(m2.tmplFilter, "\n\r\x1b") {
		t.Fatalf("tmplFilter kept a control byte: %q", m2.tmplFilter)
	}
	for _, l := range m2.addPickerLines(m2.tmplCursor) {
		if strings.Contains(l, "\n") {
			t.Fatalf("a rendered line spans rows, which breaks the box: %q", l)
		}
	}
}
