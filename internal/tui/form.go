package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/userops"
)

// fieldKind distinguishes a free-text input, a boolean toggle, and an action
// row (a focusable label the parent model activates on enter — e.g. the edit
// form's "Manage API keys →" row, which opens screenKeys).
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldToggle
	fieldAction
)

// formField is one row of a form. For fieldText the value lives in input;
// for fieldToggle it lives in on; fieldAction rows carry only a label.
type formField struct {
	key   string // logical key (base_url, api_key, enabled, …)
	label string
	kind  fieldKind
	input textinput.Model // used when kind == fieldText
	on    bool            // used when kind == fieldToggle
}

// form is a tiny multi-field wizard built on bubbles/textinput. Focus walks
// the fields top-to-bottom and then lands on a synthetic submit button
// (focus == len(fields)). It is fully synchronous and self-contained so the
// parent model can drive it with key messages and unit tests can assert on it
// without a running tea.Program.
type form struct {
	title  string
	intro  string
	submit string // submit button label, e.g. "Add" / "Save"
	fields []formField
	focus  int    // 0..len(fields)-1 = a field; len(fields) = the submit button
	err    string // validation message shown beneath the form
}

// newTextInput builds a textinput pre-populated with value. password fields
// echo a bullet so API keys aren't shown on screen.
func newTextInput(value, placeholder string, password bool) textinput.Model {
	ti := textinput.New()
	ti.Prompt = "" // the card's key column replaces the "> " prompt
	ti.SetValue(value)
	ti.Placeholder = placeholder
	ti.CharLimit = 1024
	ti.Width = 48
	if password {
		ti.EchoMode = textinput.EchoPassword
		ti.EchoCharacter = '•'
	}
	return ti
}

// newAddForm builds the add wizard, prefilled from a vendor template. A zero
// Template (the "Custom" choice) yields blank fields the user fills entirely.
// Field order: name → base_url → models_endpoint → api_key → default_model.
func newAddForm(t Template) form {
	f := form{
		title:  "Add provider",
		intro:  "↑/↓ or tab move · enter advances · enter on [Add] submits · esc cancels",
		submit: "Add",
		fields: []formField{
			{key: "name", label: "Name", kind: fieldText, input: newTextInput(t.Name, "provider id, e.g. deepseek", false)},
			{key: "base_url", label: "Base URL", kind: fieldText, input: newTextInput(t.BaseURL, "https://…/anthropic", false)},
			{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(t.ModelsEndpoint, "https://…/v1/models", false)},
			{key: "api_key", label: "API key", kind: fieldText, input: newTextInput("", "stored at <name>.key (mode 0600)", true)},
			{key: "default_model", label: "Default model", kind: fieldText, input: newTextInput(t.DefaultModel, "model id", false)},
		},
	}
	f.setFocus(0)
	return f
}

// newEditForm builds the edit wizard, prefilled from the vendor's current
// row. API-key rotation is intentionally out of scope here (it's a separate
// secret-backend concern); edit covers the vendors.toml fields userops.Edit
// accepts plus the enabled toggle.
func newEditForm(v userops.VendorView) form {
	f := form{
		title:  "Edit provider: " + v.Name,
		intro:  "↑/↓ or tab move · space toggles Enabled · enter on [Save] submits · esc cancels",
		submit: "Save",
		fields: []formField{
			{key: "base_url", label: "Base URL", kind: fieldText, input: newTextInput(v.BaseURL, "https://…/anthropic", false)},
			{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(v.ModelsEndpoint, "https://…/v1/models", false)},
			{key: "default_model", label: "Default model", kind: fieldText, input: newTextInput(v.DefaultModel, "model id", false)},
			{key: "enabled", label: "Enabled", kind: fieldToggle, on: v.Enabled},
			{key: "manage_keys", label: "Manage API keys →", kind: fieldAction},
		},
	}
	f.setFocus(0)
	return f
}

// setFocus moves focus to index i (clamped to [0, len(fields)]) and keeps the
// textinput focus state in sync so only the active text field shows a cursor.
func (f *form) setFocus(i int) {
	if i < 0 {
		i = 0
	}
	if i > len(f.fields) {
		i = len(f.fields)
	}
	f.focus = i
	for idx := range f.fields {
		if f.fields[idx].kind != fieldText {
			continue
		}
		if idx == i {
			f.fields[idx].input.Focus()
		} else {
			f.fields[idx].input.Blur()
		}
	}
}

// Update advances the form by one key message. It returns the updated form, an
// optional tea.Cmd (textinput cursor blink), and submitted=true when the user
// activated the submit button. The caller owns esc/cancel handling.
func (f form) Update(msg tea.KeyMsg) (form, tea.Cmd, bool) {
	switch msg.String() {
	case "up", "shift+tab":
		f.setFocus(f.focus - 1)
		return f, nil, false
	case "down", "tab":
		f.setFocus(f.focus + 1)
		return f, nil, false
	case "enter":
		if f.focus == len(f.fields) {
			return f, nil, true
		}
		f.setFocus(f.focus + 1)
		return f, nil, false
	}

	// On a field: toggles consume space/left/right; text fields get the key;
	// action rows swallow everything else (the parent handles their enter).
	if f.focus < len(f.fields) {
		fld := &f.fields[f.focus]
		switch fld.kind {
		case fieldToggle:
			switch msg.String() {
			case " ", "space", "left", "right":
				fld.on = !fld.on
			}
			return f, nil, false
		case fieldAction:
			return f, nil, false
		default: // fieldText
			var cmd tea.Cmd
			fld.input, cmd = fld.input.Update(msg)
			return f, cmd, false
		}
	}
	return f, nil, false
}

// value returns the trimmed text of a text field by key ("" if absent).
func (f form) value(key string) string {
	for _, fld := range f.fields {
		if fld.key == key && fld.kind == fieldText {
			return strings.TrimSpace(fld.input.Value())
		}
	}
	return ""
}

// focusedKey returns the key of the currently focused field, or "" when focus
// is on the submit button. The parent model uses this to special-case the
// default_model field (enter opens the model picker there).
func (f form) focusedKey() string {
	if f.focus < 0 || f.focus >= len(f.fields) {
		return ""
	}
	return f.fields[f.focus].key
}

// setValue overwrites the text of the field identified by key (no-op if the
// key is absent or not a text field). The model picker uses it to write the
// chosen model id back into the default_model input.
func (f *form) setValue(key, val string) {
	for i := range f.fields {
		if f.fields[i].key == key && f.fields[i].kind == fieldText {
			f.fields[i].input.SetValue(val)
			return
		}
	}
}

// boolValue returns the state of a toggle field by key (false if absent).
func (f form) boolValue(key string) bool {
	for _, fld := range f.fields {
		if fld.key == key && fld.kind == fieldToggle {
			return fld.on
		}
	}
	return false
}

// cardKey maps a field to the config card's short key column, so the edit card lines up
// with the read-only preview (the same grammar, editable values).
func cardKey(key string) string {
	switch key {
	case "base_url":
		return "base url"
	case "models_endpoint":
		return "models"
	case "default_model":
		return "default"
	case "api_key":
		return "key"
	case "manage_keys":
		return "keys"
	}
	return key // name / enabled already read as card keys
}

// viewLines renders the form in the read-only config card's grammar — the live status
// line, a "Config" section, then one "key  value" row per field — so entering edit barely
// reshapes the pane. Values always render through input.View() (the focused input draws
// its cursor; a password field stays bullet-masked).
func (f form) viewLines() []string {
	var lines []string
	// The edit form's enabled toggle mirrors the preview card's status line, live.
	for _, fld := range f.fields {
		if fld.kind != fieldToggle || fld.key != "enabled" {
			continue
		}
		status := okStyle.Render("● enabled")
		if !fld.on {
			status = faintStyle.Render("○ disabled")
		}
		if dm := f.value("default_model"); dm != "" {
			status += faintStyle.Render(" · " + trunc(dm, 28))
		}
		lines = append(lines, status, "")
		break
	}
	lines = append(lines, faintStyle.Render("Config"))
	for i, fld := range f.fields {
		focused := i == f.focus
		key := fmt.Sprintf("%-8s", cardKey(fld.key))
		keyCell := faintStyle.Render(key)
		if focused {
			keyCell = selectedStyle.Render(key)
		}
		switch fld.kind {
		case fieldText:
			lines = append(lines, " "+keyCell+"  "+fld.input.View())
		case fieldToggle:
			state := "[ ] off"
			if fld.on {
				state = "[x] on"
			}
			lines = append(lines, " "+keyCell+"  "+contentStyle.Render(state))
		case fieldAction:
			// Value-column action label; enter on it is handled by the parent
			// model (e.g. open the key manager).
			label := contentStyle.Render(fld.label)
			if focused {
				label = selectedStyle.Render(fld.label)
			}
			lines = append(lines, " "+keyCell+"  "+label)
		}
	}
	btn := "   [ " + f.submit + " ]"
	if f.focus == len(f.fields) {
		btn = " " + cursorStyle.Render("❯ ") + selectedStyle.Render("[ "+f.submit+" ]")
	}
	lines = append(lines, "", btn)
	if f.err != "" {
		lines = append(lines, "", errStyle.Render(f.err))
	}
	// The focused field's note renders in its own bottom section (Config-grammar
	// aligned), so moving focus never reflows the field rows above.
	if f.focus < len(f.fields) {
		fld := f.fields[f.focus]
		if fld.key == "default_model" && f.value("models_endpoint") != "" {
			lines = append(lines, "", faintStyle.Render("Note"),
				faintStyle.Render(" enter picks from the provider's model list"))
		}
	}
	return lines
}
