package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/permmode"
	"github.com/ethanhq/cc-fleet/internal/userops"
)

// effortChoices / permChoices are the static edit-form dropdown vocabularies;
// the leading "off" maps to an unset field on submit. Built from the config /
// permmode sources so the dropdowns can't drift from the validators.
var (
	effortChoices = append([]string{"off"}, config.EffortLevels()...)
	permChoices   = append([]string{"off"}, permmode.Modes...)
)

// isModelSlotKey reports whether key is one of the three model-slot text fields
// (default/strong/fast) — the fields that open the model picker on enter.
func isModelSlotKey(key string) bool {
	return key == "default_model" || key == "strong_model" || key == "fast_model"
}

// orOff maps an unset config value to the "off" label — the dropdown option in the
// edit form and the displayed value in the read-only preview card.
func orOff(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

// fieldKind distinguishes a free-text input, a boolean toggle, and an action
// row (a focusable label the parent model activates on enter — e.g. the edit
// form's "Manage API keys →" row, which opens screenKeys).
type fieldKind int

const (
	fieldText fieldKind = iota
	fieldToggle
	fieldAction
	fieldChoice
)

// formField is one row of a form. For fieldText the value lives in input;
// for fieldToggle it lives in on; for fieldChoice it is choices[choiceIdx];
// fieldAction rows carry only a label.
type formField struct {
	key       string // logical key (base_url, api_key, enabled, …)
	label     string
	kind      fieldKind
	input     textinput.Model // used when kind == fieldText
	on        bool            // used when kind == fieldToggle
	choices   []string        // used when kind == fieldChoice (cycled with space/←/→)
	choiceIdx int             // used when kind == fieldChoice
}

// newChoiceField builds a choice row pre-selected to current (the first choice
// when current isn't in the set).
func newChoiceField(key, label string, choices []string, current string) formField {
	idx := 0
	for i, c := range choices {
		if c == current {
			idx = i
			break
		}
	}
	return formField{key: key, label: label, kind: fieldChoice, choices: choices, choiceIdx: idx}
}

// form is a tiny multi-field wizard built on bubbles/textinput. Focus walks
// the fields top-to-bottom and then lands on a synthetic submit button
// (focus == len(fields)). It is fully synchronous and self-contained so the
// parent model can drive it with key messages and unit tests can assert on it
// without a running tea.Program.
type form struct {
	title      string
	intro      string
	submit     string // submit button label, e.g. "Add" / "Save"
	statusNote string // optional banner above the fields (e.g. the codex login source)
	isDefault  bool   // this provider is the effective default → the status line notes it
	fields     []formField
	focus      int    // 0..len(fields)-1 = a field; len(fields) = the submit button
	err        string // validation message shown beneath the form
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

// newAddForm builds the add wizard, prefilled from a provider template. A zero
// Template (the "Custom" choice) yields blank fields the user fills entirely.
// Field order: name → base_url → models_endpoint → api_key → the model-config rows
// (default/strong/fast + 1M toggles, effort, default permission).
func newAddForm(t Template) form {
	fields := []formField{
		{key: "name", label: "Name", kind: fieldText, input: newTextInput(t.Name, "provider id, e.g. deepseek", false)},
		{key: "base_url", label: "Base URL", kind: fieldText, input: newTextInput(t.BaseURL, "https://…/anthropic", false)},
		{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(t.ModelsEndpoint, "https://…/v1/models", false)},
		{key: "api_key", label: "API key", kind: fieldText, input: newTextInput("", "stored at <name>.key (mode 0600)", true)},
	}
	fields = append(fields, modelConfigFields(t.DefaultModel, "", "", "", "")...)
	f := form{
		title:  "Add provider",
		intro:  "↑/↓ move rows · → 1M toggle · enter on [Add] submits · esc cancels",
		submit: "Add",
		fields: fields,
	}
	f.setFocus(0)
	return f
}

// newOpenAIAddForm builds the OpenAI-protocol add wizard. The loopback base_url
// and the protocol are assigned on submit, so the form collects only the real
// upstream + key; models_endpoint is prefilled from the upstream base.
func newOpenAIAddForm(t OAITemplate) form {
	models := ""
	if t.UpstreamURL != "" {
		models = strings.TrimRight(t.UpstreamURL, "/") + "/models"
	}
	fields := []formField{
		{key: "name", label: "Name", kind: fieldText, input: newTextInput(t.Name, "provider id, e.g. openai", false)},
		{key: "upstream_url", label: "Upstream URL", kind: fieldText, input: newTextInput(t.UpstreamURL, "https://api.openai.com/v1", false)},
		{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(models, "https://…/v1/models", false)},
		{key: "api_key", label: "API key", kind: fieldText, input: newTextInput("", "stored at <name>.key (mode 0600)", true)},
	}
	fields = append(fields, modelConfigFields(t.DefaultModel, "", "", "", "")...)
	f := form{
		title:  "Add OpenAI provider",
		intro:  "↑/↓ move rows · → 1M toggle · enter on [Add] submits · esc cancels",
		submit: "Add",
		fields: fields,
	}
	f.setFocus(0)
	return f
}

// newCodexAddForm builds the minimal codex add wizard. The loopback base_url +
// models_endpoint and the codex-oauth backend are assigned on submit; the
// upstream is the fixed ChatGPT backend, so there is no key or URL to enter.
func newCodexAddForm(name, defaultModel, statusNote string) form {
	fields := []formField{
		{key: "name", label: "Name", kind: fieldText, input: newTextInput(name, "provider id", false)},
	}
	fields = append(fields, modelConfigFields(defaultModel, "", "", "", "")...)
	f := form{
		title:      "Add codex provider",
		intro:      "↑/↓ move rows · → 1M toggle · enter on [Add] submits · esc cancels",
		submit:     "Add",
		statusNote: statusNote,
		fields:     fields,
	}
	f.setFocus(0)
	return f
}

// modelConfigFields builds the shared model-roster rows used by the edit form and
// the three add forms: default/strong/fast model slots (each a bare-id text field
// with an inline [1m] toggle, recombined on submit) plus the effort + run-permission
// choice dropdowns. One source so the add/edit dropdown vocab + nav can't drift.
func modelConfigFields(defModel, strong, fast, effort, perm string) []formField {
	return []formField{
		{key: "default_model", label: "Default model", kind: fieldText, input: newTextInput(config.Strip1M(defModel), "model id", false)},
		{key: "default_1m", label: "1M context", kind: fieldToggle, on: config.Has1M(defModel)},
		{key: "strong_model", label: "Strong model", kind: fieldText, input: newTextInput(config.Strip1M(strong), "blank → follows default", false)},
		{key: "strong_1m", label: "1M context", kind: fieldToggle, on: config.Has1M(strong)},
		{key: "fast_model", label: "Fast model", kind: fieldText, input: newTextInput(config.Strip1M(fast), "blank → follows default", false)},
		{key: "fast_1m", label: "1M context", kind: fieldToggle, on: config.Has1M(fast)},
		newChoiceField("effort", "Effort", effortChoices, orOff(effort)),
		newChoiceField("permission", "Run perm", permChoices, orOff(perm)),
	}
}

// newEditForm builds the edit wizard, prefilled from the provider's current row.
// The editable endpoint depends on the class: an Anthropic-native provider edits its
// real base_url; an openai-* provider edits its real upstream_url (base_url is the
// internal loopback daemon); codex has neither (its endpoints are loopback + the
// upstream is the fixed ChatGPT backend). codex authenticates via OAuth, so it has
// no API key set to rotate and the key manager is omitted.
func newEditForm(v userops.ProviderView) form {
	var fields []formField
	switch v.Protocol {
	case config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses:
		fields = append(fields,
			formField{key: "upstream_url", label: "Upstream URL", kind: fieldText, input: newTextInput(v.UpstreamURL, "https://api.openai.com/v1", false)},
			formField{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(v.ModelsEndpoint, "https://…/v1/models", false)})
	case config.ProtocolCodexOAuth:
		// loopback base_url + models_endpoint are internal; nothing to edit there.
	default:
		fields = append(fields,
			formField{key: "base_url", label: "Base URL", kind: fieldText, input: newTextInput(v.BaseURL, "https://…/anthropic", false)},
			formField{key: "models_endpoint", label: "Models endpoint", kind: fieldText, input: newTextInput(v.ModelsEndpoint, "https://…/v1/models", false)})
	}
	fields = append(fields, modelConfigFields(v.DefaultModel, v.StrongModel, v.FastModel, v.Effort, v.DefaultPerm)...)
	fields = append(fields, formField{key: "enabled", label: "Enabled", kind: fieldToggle, on: v.Enabled})
	if v.SecretBackend != config.CodexOAuthBackend {
		fields = append(fields, formField{key: "manage_keys", label: "Manage API keys →", kind: fieldAction})
	}
	f := form{
		title:     "Edit provider: " + v.Name,
		intro:     "↑/↓ move rows · → 1M toggle · space toggles · enter on [Save] submits · esc cancels",
		submit:    "Save",
		isDefault: v.Default,
		fields:    fields,
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
		f.setFocus(f.stepFocus(f.focus, -1))
		return f, nil, false
	case "down", "tab":
		f.setFocus(f.stepFocus(f.focus, +1))
		return f, nil, false
	case "enter":
		if f.focus == len(f.fields) {
			return f, nil, true
		}
		if f.fields[f.focus].kind == fieldToggle {
			f.fields[f.focus].on = !f.fields[f.focus].on
			return f, nil, false
		}
		f.setFocus(f.stepFocus(f.focus, +1))
		return f, nil, false
	case "right":
		// → on a model slot reaches its inline 1M toggle when that slot has one;
		// otherwise it falls through to the text cursor (a choice cycles below).
		if mk := oneMKeyFor(f.focusedKey()); mk != "" {
			if idx, ok := f.indexOfKey(mk); ok {
				f.setFocus(idx)
				return f, nil, false
			}
		}
	case "left":
		// ← on a 1M toggle returns to its model slot (a choice cycles instead).
		if mk := modelKeyFor1m(f.focusedKey()); mk != "" {
			if idx, ok := f.indexOfKey(mk); ok {
				f.setFocus(idx)
				return f, nil, false
			}
		}
	}

	// On a field: a toggle flips on space; a choice cycles on space/←/→; text
	// fields get the key; action rows swallow everything else (the parent handles
	// their enter).
	if f.focus < len(f.fields) {
		fld := &f.fields[f.focus]
		switch fld.kind {
		case fieldToggle:
			switch msg.String() {
			case " ", "space":
				fld.on = !fld.on
			}
			return f, nil, false
		case fieldChoice:
			switch msg.String() {
			case " ", "space", "right":
				fld.choiceIdx = (fld.choiceIdx + 1) % len(fld.choices)
			case "left":
				fld.choiceIdx = (fld.choiceIdx - 1 + len(fld.choices)) % len(fld.choices)
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

// focusedText reports whether focus sits on a text field (whose input consumes
// the arrow keys for cursor movement).
func (f form) focusedText() bool {
	return f.focus < len(f.fields) && f.fields[f.focus].kind == fieldText
}

// focusedChoice reports whether focus sits on a choice field (which consumes ←/→
// to cycle its options rather than letting ← navigate back to the list).
func (f form) focusedChoice() bool {
	return f.focus < len(f.fields) && f.fields[f.focus].kind == fieldChoice
}

// oneMKeyFor maps a model-slot text field to its inline 1M-context toggle key, or
// "" for a non-slot field. The toggle renders as a trailing column on the model
// row (see viewLines) instead of its own line, and is reached laterally with →.
func oneMKeyFor(modelKey string) string {
	switch modelKey {
	case "default_model":
		return "default_1m"
	case "strong_model":
		return "strong_1m"
	case "fast_model":
		return "fast_1m"
	}
	return ""
}

// modelKeyFor1m is the inverse of oneMKeyFor: the model-slot key a 1M toggle
// belongs to, or "" when key isn't a 1M toggle.
func modelKeyFor1m(oneMKey string) string {
	switch oneMKey {
	case "default_1m":
		return "default_model"
	case "strong_1m":
		return "strong_model"
	case "fast_1m":
		return "fast_model"
	}
	return ""
}

func isOneMKey(key string) bool { return modelKeyFor1m(key) != "" }

// focusedOneM reports whether focus sits on an inline 1M toggle (reached via →,
// returned from via ←) — so the parent's ← "back to list" doesn't fire there.
func (f form) focusedOneM() bool { return isOneMKey(f.focusedKey()) }

// keyAt returns the field key at idx (or "" for the submit row / out of range).
func (f form) keyAt(idx int) string {
	if idx < 0 || idx >= len(f.fields) {
		return ""
	}
	return f.fields[idx].key
}

// indexOfKey returns the field index for key and whether it exists — not every form
// instance carries every field (a model slot may have no paired 1M toggle), so
// callers must guard on the bool rather than mis-focusing a not-found key.
func (f form) indexOfKey(key string) (int, bool) {
	for i := range f.fields {
		if f.fields[i].key == key {
			return i, true
		}
	}
	return 0, false
}

// stepFocus returns the next focus index moving by dir (±1). The form is a small
// grid: a model column (with the single-control rows) and a parallel 1M-context
// column for the model rows. ↑/↓ stay in their column — from a 1M toggle they land
// on the next row's 1M toggle (or fall back to the model column when the next row
// has none); → / ← switch columns. The 1M toggles are skipped when walking the
// model column.
func (f form) stepFocus(from, dir int) int {
	inCtx := isOneMKey(f.keyAt(from))
	cur := from
	if mk := modelKeyFor1m(f.keyAt(from)); mk != "" {
		if idx, ok := f.indexOfKey(mk); ok {
			cur = idx // navigate from the toggle's model row
		}
	}
	next := -1
	for i := cur + dir; i >= 0 && i < len(f.fields); i += dir {
		if !isOneMKey(f.fields[i].key) {
			next = i
			break
		}
	}
	if next < 0 {
		if cur+dir >= len(f.fields) {
			return len(f.fields) // the submit button
		}
		return 0
	}
	// Vertical nav keeps the 1M-context column when the next row also has one.
	if inCtx {
		if mk := oneMKeyFor(f.keyAt(next)); mk != "" {
			if idx, ok := f.indexOfKey(mk); ok {
				return idx
			}
		}
	}
	return next
}

// render1MTag draws a model slot's 1M-context toggle as a trailing tag, lit when
// its toggle field holds focus.
func (f form) render1MTag(key string) string {
	on, focused := false, false
	for i, fld := range f.fields {
		if fld.key == key {
			on, focused = fld.on, i == f.focus
			break
		}
	}
	state := "[ ]"
	if on {
		state = "[x]"
	}
	tag := "1M ctx " + state
	if focused {
		return selectedStyle.Render(tag)
	}
	return faintStyle.Render(tag)
}

// fieldNote is the focused field's contextual explanation, shown in the Note
// section above the Config rows — what each slot is for, not just how to edit it.
func fieldNote(key string) string {
	switch key {
	case "default_model":
		return "default — the model teammates and subagents run on. enter picks from the provider's model list."
	case "strong_model":
		return "strong — for heavier reasoning and plan mode; blank follows default. enter picks from the list."
	case "fast_model":
		return "fast — background work (titles, context compaction) and light calls; blank follows default. enter picks from the list."
	case "default_1m", "strong_1m", "fast_1m":
		return "1M context — mark this model's window as 1M; the marker is stripped before the request reaches the provider."
	case "effort":
		return "reasoning effort — needs provider support, else requests may error. space / ←/→ cycles."
	case "permission":
		return "default permission mode for `cc-fleet run`. space / ←/→ cycles."
	case "enabled":
		return "enter / space toggles whether the provider is enabled."
	case "manage_keys":
		return "enter opens the per-provider key manager."
	}
	return ""
}

// fieldNoteKeys enumerates the keys fieldNote returns a non-empty note for, so the
// per-width note reserve can find the tallest wrap (default_1m stands in for all
// three 1M toggles — they share a note).
var fieldNoteKeys = []string{
	"default_model", "strong_model", "fast_model", "default_1m", "effort", "permission", "enabled", "manage_keys",
}

// fieldNoteReserve is the tallest a field note wraps to at this width (≥1). The edit
// form and the read-only preview card both pad their Note body to it so the Config
// block sits at the same row in both — entering edit doesn't shift Config down.
func fieldNoteReserve(width int) int {
	r := 1
	for _, k := range fieldNoteKeys {
		if n := len(wrapTo(fieldNote(k), width-2)); n > r {
			r = n
		}
	}
	return r
}

// noteReserve is the fixed line count the Note body occupies at this width: the
// tallest wrap of the form's statusNote (codex source), or of any fieldNote, so the
// Config block stays at a constant row as focus moves (no "jump"). At least 1.
func (f form) noteReserve(width int) int {
	if f.statusNote != "" {
		return max(1, len(wrapTo(f.statusNote, width-2)))
	}
	return fieldNoteReserve(width)
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

// choiceValue returns the selected option of a choice field by key ("" if absent).
func (f form) choiceValue(key string) string {
	for _, fld := range f.fields {
		if fld.key == key && fld.kind == fieldChoice && fld.choiceIdx >= 0 && fld.choiceIdx < len(fld.choices) {
			return fld.choices[fld.choiceIdx]
		}
	}
	return ""
}

// cardKey maps a field to the config card's short key column, so the edit card lines up
// with the read-only preview (the same grammar, editable values).
func cardKey(key string) string {
	switch key {
	case "base_url":
		return "base url"
	case "upstream_url":
		return "upstream"
	case "models_endpoint":
		return "models"
	case "default_model":
		return "default"
	case "strong_model":
		return "strong"
	case "fast_model":
		return "fast"
	case "effort":
		return "effort"
	case "permission":
		return "run perm"
	case "api_key":
		return "key"
	case "manage_keys":
		return "keys"
	}
	return key // name / enabled already read as card keys
}

// viewLines renders the form in the read-only config card's grammar — a "Config" section
// of "key  value" rows (the edit form adds the live status line its enabled toggle drives)
// — so entering edit barely reshapes the pane. Text-field values always render through
// input.View() (the focused input draws its cursor; a password field stays bullet-masked).
func (f form) viewLines(width int) []string {
	var lines []string
	// The edit form's enabled toggle mirrors the preview card's status line, live.
	for _, fld := range f.fields {
		if fld.kind != fieldToggle || fld.key != "enabled" {
			continue
		}
		status := okStyle.Render("● enabled")
		if !fld.on {
			status = liveStyle.Render("○ disabled")
		}
		if dm := f.value("default_model"); dm != "" {
			status += liveStyle.Render(" · " + trunc(dm, 28))
		}
		if f.isDefault {
			status += noteStyle.Render(" · default")
		}
		lines = append(lines, status, "")
		break
	}
	// The Note section sits ABOVE the fields. A form-level statusNote (e.g. which
	// codex login a codex provider reuses) takes it and wraps to the pane width;
	// otherwise it shows the focused field's contextual hint.
	lines = append(lines, contentStyle.Render("Note"))
	// Build the Note body + its style, then pad it to a fixed reserve = the tallest
	// any note wraps to at this width (see noteReserve), so the Config block sits at
	// a constant row as focus moves — no "jump" — and nothing is truncated.
	var body []string
	bodyStyle := faintStyle
	if f.statusNote != "" {
		body, bodyStyle = wrapTo(f.statusNote, width-2), okStyle
	} else {
		focusedKey := ""
		if f.focus < len(f.fields) {
			focusedKey = f.fields[f.focus].key
		}
		if n := fieldNote(focusedKey); n != "" {
			body, bodyStyle = wrapTo(n, width-2), noteStyle
		} else {
			body = []string{"—"}
		}
	}
	for i, reserve := 0, f.noteReserve(width); i < reserve; i++ {
		if i < len(body) {
			lines = append(lines, " "+bodyStyle.Render(body[i]))
		} else {
			lines = append(lines, "")
		}
	}
	lines = append(lines, "", faintStyle.Render("Config"))
	for i, fld := range f.fields {
		// A model slot's 1M toggle renders as a trailing column on the model row
		// (below), not its own line — skip it in the per-field loop.
		if fld.key == "default_1m" || fld.key == "strong_1m" || fld.key == "fast_1m" {
			continue
		}
		focused := i == f.focus
		key := fmt.Sprintf("%-8s", cardKey(fld.key))
		keyCell := faintStyle.Render(key)
		if focused {
			keyCell = selectedStyle.Render(key)
		}
		switch fld.kind {
		case fieldText:
			line := " " + keyCell + "  " + fld.input.View()
			// A model slot trails its 1M-context toggle on the same row, when that
			// slot has one.
			if mk := oneMKeyFor(fld.key); mk != "" {
				if _, ok := f.indexOfKey(mk); ok {
					line += "   " + f.render1MTag(mk)
				}
			}
			lines = append(lines, line)
		case fieldToggle:
			state := "[ ] off"
			if fld.on {
				state = "[x] on"
			}
			lines = append(lines, " "+keyCell+"  "+contentStyle.Render(state))
		case fieldChoice:
			val := ""
			if fld.choiceIdx >= 0 && fld.choiceIdx < len(fld.choices) {
				val = fld.choices[fld.choiceIdx]
			}
			st := contentStyle
			if fld.key == "permission" {
				st = permModeStyle(val) // the Claude permission-indicator palette
			}
			lines = append(lines, " "+keyCell+"  "+st.Render("‹ "+val+" ›"))
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
	return lines
}
