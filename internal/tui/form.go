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
		title:  "Add vendor",
		intro:  "↑/↓ or tab move · enter advances · enter on [Add] submits · esc cancels",
		submit: "Add",
		fields: []formField{
			{key: "name", label: "Name", kind: fieldText, input: newTextInput(t.Name, "vendor id, e.g. deepseek", false)},
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
		title:  "Edit vendor: " + v.Name,
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

// View renders the form. Field labels are padded to a fixed column so inputs
// align; the focused row and submit button are highlighted.
func (f form) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.title) + "\n")
	if f.intro != "" {
		b.WriteString(faintStyle.Render(f.intro) + "\n")
	}
	b.WriteString("\n")

	for i, fld := range f.fields {
		cursor := "  "
		if i == f.focus {
			cursor = cursorStyle.Render("> ")
		}
		label := fmt.Sprintf("%-16s", fld.label)
		switch fld.kind {
		case fieldText:
			b.WriteString(cursor + label + " " + fld.input.View() + "\n")
			// Tell the user the default_model field can pull the list from the
			// vendor (only meaningful when there's an endpoint to hit).
			if fld.key == "default_model" && f.value("models_endpoint") != "" {
				b.WriteString(faintStyle.Render("                   enter: pick from vendor's model list") + "\n")
			}
		case fieldToggle:
			state := "[ ] off"
			if fld.on {
				state = "[x] on"
			}
			b.WriteString(cursor + label + " " + state + "\n")
		case fieldAction:
			// Standalone action label (no value column); enter on it is handled
			// by the parent model (e.g. open the key manager).
			line := fld.label
			if i == f.focus {
				line = selectedStyle.Render(fld.label)
			}
			b.WriteString(cursor + line + "\n")
		}
	}

	btn := "[ " + f.submit + " ]"
	if f.focus == len(f.fields) {
		b.WriteString("\n" + cursorStyle.Render("> ") + selectedStyle.Render(btn) + "\n")
	} else {
		b.WriteString("\n" + "  " + btn + "\n")
	}
	if f.err != "" {
		b.WriteString("\n" + errStyle.Render(f.err) + "\n")
	}
	return b.String()
}
