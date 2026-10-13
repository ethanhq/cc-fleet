package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

func extraBodyView(protocol, extra string) userops.ProviderView {
	return userops.ProviderView{
		Name: "oai", Protocol: protocol, BaseURL: "http://127.0.0.1:17240/",
		UpstreamURL: "https://openrouter.ai/api/v1", ModelsEndpoint: "https://openrouter.ai/api/v1/models",
		DefaultModel: "deepseek/deepseek-v4.1-flash", SecretBackend: "file", Enabled: true, ExtraBody: extra,
	}
}

// Only the openai-* edit forms carry the extra_body row: prefilled, right after the
// models endpoint, and long enough for the largest valid value.
func TestEditForm_ExtraBodyFieldByClass(t *testing.T) {
	const eb = `{"provider":{"only":["deepseek"]}}`
	for _, p := range []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses} {
		f := newEditForm(extraBodyView(p, eb))
		idx, ok := f.indexOfKey("extra_body")
		if mi, _ := f.indexOfKey("models_endpoint"); !ok || idx != mi+1 {
			t.Fatalf("%s: extra_body row present=%v at %d, want right after models_endpoint (%d)", p, ok, idx, mi)
		}
		if got, limit := f.value("extra_body"), f.fields[idx].input.CharLimit; got != eb || limit != config.MaxExtraBodyBytes {
			t.Fatalf("%s: extra_body prefill %q (want %q), CharLimit %d (want %d)", p, got, eb, limit, config.MaxExtraBodyBytes)
		}
	}
	for name, f := range map[string]form{
		"native edit": newEditForm(userops.ProviderView{Name: "deepseek", SecretBackend: "file"}),
		"codex edit":  newEditForm(userops.ProviderView{Name: "codex", Protocol: config.ProtocolCodexOAuth, SecretBackend: config.CodexOAuthBackend}),
		"native add":  newAddForm(Templates[0]),
		"openai add":  newOpenAIAddForm(OAITemplates[0]),
	} {
		if _, ok := f.indexOfKey("extra_body"); ok {
			t.Errorf("the %s form must not have an extra_body row", name)
		}
	}
}

// The row sits under the short "extra" key, and its note renders whole without raising
// the height the form reserves for notes.
func TestEditForm_ExtraBodyRowAndNote(t *testing.T) {
	note := fieldNote("extra_body")
	if !strings.Contains(note, "Not for secrets") {
		t.Fatalf("extra_body note = %q", note)
	}
	for _, w := range []int{60, 80} {
		want := 1 // the reserve without the new key
		for _, k := range fieldNoteKeys {
			if k != "extra_body" {
				want = max(want, len(wrapTo(fieldNote(k), w-2)))
			}
		}
		if got := fieldNoteReserve(w); got != want {
			t.Errorf("width %d: fieldNoteReserve = %d, want %d (unchanged by extra_body)", w, got, want)
		}

		f := newEditForm(extraBodyView(config.ProtocolOpenAIChat, ""))
		idx, _ := f.indexOfKey("extra_body")
		f.setFocus(idx)
		var rendered []string
		for _, l := range f.viewLines(w) {
			rendered = append(rendered, strings.TrimSpace(ansi.Strip(l)))
		}
		for _, chunk := range wrapTo(note, w-2) {
			if !slices.Contains(rendered, chunk) {
				t.Errorf("width %d: note line %q is missing from the rendered form:\n%s", w, chunk, strings.Join(rendered, "\n"))
			}
		}
		// The key, then the input (its placeholder while empty).
		if !slices.ContainsFunc(rendered, func(l string) bool {
			return strings.HasPrefix(l, "extra ") && strings.Contains(l, `{"provider"`)
		}) {
			t.Errorf("width %d: no `extra  <value>` row in the rendered form:\n%s", w, strings.Join(rendered, "\n"))
		}
	}
}

// editModel returns a Model on the edit form for view, entered from the provider list.
func editModel(t *testing.T, view userops.ProviderView) Model {
	t.Helper()
	m, _ := press(t, withProviders(t, view), "enter")
	if m.screen != screenForm || m.formMode != modeEdit {
		t.Fatalf("enter on the provider did not open its edit form (screen=%d)", m.screen)
	}
	return m
}

// A bad extra_body keeps the user on the form with the reason shown and the typed text
// intact; nothing is dispatched. The row is checked against the form's own protocol.
func TestSubmitEdit_ExtraBodyInvalidStaysOnForm(t *testing.T) {
	for _, c := range []struct{ protocol, value, wantErr string }{
		{config.ProtocolOpenAIChat, `{"provider":`, "extra_body must be a JSON object"},
		{config.ProtocolOpenAIChat, `{"Model":"x"}`, `extra_body must not set "model"`},
		{config.ProtocolOpenAIResponses, `{"input":[]}`, `extra_body must not set "input"`}, // allowed on openai-chat
		// CharLimit counts characters, so only multi-byte text reaches the byte limit.
		{config.ProtocolOpenAIChat, `{"a":"` + strings.Repeat("世", 1500) + `"}`, "over the 4096-byte limit"},
	} {
		m := editModel(t, extraBodyView(c.protocol, ""))
		m.form.setValue("extra_body", c.value)
		nm, cmd := m.submitEdit()
		got := nm.(Model)
		if cmd != nil || got.screen != screenForm || !strings.Contains(got.form.err, c.wantErr) {
			t.Errorf("%s %.24q: dispatched=%v screen=%d err=%q, want to stay on the form with %q",
				c.protocol, c.value, cmd != nil, got.screen, got.form.err, c.wantErr)
		}
		if got.form.value("extra_body") != c.value {
			t.Errorf("%s %.24q: the typed value was lost", c.protocol, c.value)
		}
	}
}

// saveOAI writes an openai-chat provider "oai" with the given extra_body into the test config.
func saveOAI(t *testing.T, extra string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers["oai"] = &config.Provider{
		Name: "oai", Protocol: config.ProtocolOpenAIChat, BaseURL: "http://127.0.0.1:17240/",
		UpstreamURL: "https://openrouter.ai/api/v1", ModelsEndpoint: "https://openrouter.ai/api/v1/models",
		DefaultModel: "deepseek/deepseek-v4.1-flash", SecretBackend: "file", SecretRef: "oai.key", Enabled: true,
		ExtraBody: extra,
	}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
}

// editStoredOAI opens the edit form on the stored provider "oai", applies change to the
// form, presses [Save], runs the edit it dispatches and returns the provider as stored.
func editStoredOAI(t *testing.T, change func(*form)) *config.Provider {
	t.Helper()
	res, err := userops.List()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Providers, func(v userops.ProviderView) bool { return v.Name == "oai" })
	if i < 0 {
		t.Fatal("provider oai not listed")
	}
	m := editModel(t, res.Providers[i])
	change(&m.form)
	m.form.setFocus(len(m.form.fields)) // the [Save] button
	m, cmd := press(t, m, "enter")
	if cmd == nil || m.form.err != "" {
		t.Fatalf("save did not dispatch (err=%q)", m.form.err)
	}
	if done, ok := cmd().(opDoneMsg); !ok || done.err != nil {
		t.Fatalf("edit failed: %+v", done)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Providers["oai"]
}

// Through the real form path: a typed value is saved to providers.toml, shows again
// when the form is reopened, and clearing the row removes it.
func TestSubmitEdit_ExtraBodySavedAndCleared(t *testing.T) {
	setupEnv(t)
	saveOAI(t, "")
	const eb = `{"provider":{"only":["deepseek"],"allow_fallbacks":false}}`
	if got := editStoredOAI(t, func(f *form) { f.setValue("extra_body", eb) }).ExtraBody; got != eb {
		t.Fatalf("saved extra_body = %q, want %q", got, eb)
	}
	got := editStoredOAI(t, func(f *form) {
		if shown := f.value("extra_body"); shown != eb {
			t.Fatalf("reopened form shows %q, want %q", shown, eb)
		}
		f.setValue("extra_body", "")
	})
	if got.ExtraBody != "" {
		t.Fatalf("extra_body after clearing = %q, want empty", got.ExtraBody)
	}
}

// The text input cannot hold every valid value: it turns newlines into spaces and drops
// U+FFFD. Saving the form with the row untouched must leave the stored value alone.
func TestSubmitEdit_ExtraBodyUntouchedRowKeepsStoredValue(t *testing.T) {
	setupEnv(t)
	const stored = "{\n  \"note\": \"a�b\"\n}\n"
	saveOAI(t, stored)
	got := editStoredOAI(t, func(f *form) {
		if shown := f.value("extra_body"); shown == stored || !strings.Contains(shown, `"note"`) {
			t.Fatalf("the row shows %q; this test needs a value the input alters", shown)
		}
		f.setToggle("enabled", false)
	})
	if got.Enabled {
		t.Fatal("the other edit (enabled off) was not saved")
	}
	if got.ExtraBody != stored {
		t.Fatalf("extra_body rewritten by an edit that did not touch it: %q, want %q", got.ExtraBody, stored)
	}
}
