package tui

import (
	"strings"
	"testing"

	"github.com/ethanhq/cc-fleet/internal/codexproxy"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

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

// With no usable codex source, the codex row routes to the consent screen; accept
// moves to the device-code stage; esc cancels back to the provider list.
func TestAddPicker_CodexRowNoSourceGoesToConsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())            // no ~/.codex
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no own login
	m := NewModel()
	m, _ = press(t, m, "enter")
	m = pressN(t, m, "down", codexIdx()) // to the codex row
	m, _ = press(t, m, "enter")
	if m.screen != screenCodexAuth || m.codexAuthStage != codexAuthConsent {
		t.Fatalf("no-source codex: screen=%d stage=%d", m.screen, m.codexAuthStage)
	}
	m, cmd := press(t, m, "enter") // accept consent -> device stage
	if m.codexAuthStage != codexAuthDevice || cmd == nil {
		t.Fatalf("consent accept: stage=%d cmd=%v", m.codexAuthStage, cmd)
	}
	m, _ = press(t, m, "esc")
	if m.screen != screenList {
		t.Fatalf("esc from device stage: screen=%d, want screenList", m.screen)
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
	f := newCodexAddForm("gpt-5.5", "reuses the codex CLI login (account …abc) — no key needed")
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
	m.form = newCodexAddForm("gpt-5.5", "")
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
// edited vendor's class (so a prior codex flow can't leak into a deepseek edit).
func TestEditForm_ModelPickerSourceByClass(t *testing.T) {
	deepseek := userops.VendorView{
		Name: "deepseek", BaseURL: "https://api.deepseek.com/anthropic",
		ModelsEndpoint: "https://api.deepseek.com/v1/models", DefaultModel: "x",
		SecretBackend: "file", Protocol: "", Enabled: true,
	}
	codex := userops.VendorView{
		Name: "codex", BaseURL: "http://127.0.0.1:17222/", ModelsEndpoint: "http://127.0.0.1:17222/v1/models",
		DefaultModel: "gpt-5.5", SecretBackend: config.CodexOAuthBackend, Protocol: config.ProtocolCodexOAuth, Enabled: true,
	}
	m := withVendors(t, deepseek, codex) // class-sorted: deepseek (0), codex (1)

	// editing codex -> static codex list, and no key-manager row.
	mc := m
	mc.screen = screenList
	mc.vendorCursor = 1
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
	md.vendorCursor = 0
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
	in := []userops.VendorView{
		{Name: "codex", Protocol: config.ProtocolCodexOAuth},
		{Name: "deepseek", Protocol: ""},
		{Name: "groq", Protocol: config.ProtocolOpenAIChat},
	}
	m, _ = step(t, m, vendorsMsg{vendors: in})
	got := []string{m.vendors[0].Name, m.vendors[1].Name, m.vendors[2].Name}
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
	oai := keys(newEditForm(userops.VendorView{Name: "openai", Protocol: config.ProtocolOpenAIResponses, UpstreamURL: "https://api.openai.com/v1", SecretBackend: "file"}))
	if !oai["upstream_url"] || oai["base_url"] || !oai["manage_keys"] {
		t.Fatalf("openai edit fields = %v (want upstream_url + keys, no base_url)", oai)
	}
	cdx := keys(newEditForm(userops.VendorView{Name: "codex", Protocol: config.ProtocolCodexOAuth, SecretBackend: config.CodexOAuthBackend}))
	if cdx["base_url"] || cdx["models_endpoint"] || cdx["upstream_url"] || cdx["manage_keys"] {
		t.Fatalf("codex edit fields = %v (want only default_model + enabled)", cdx)
	}
	ant := keys(newEditForm(userops.VendorView{Name: "deepseek", Protocol: "", SecretBackend: "file"}))
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
