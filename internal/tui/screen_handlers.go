package tui

// screen_handlers.go is a thin registry of (update, view) pairs keyed by
// screen. It does NOT replace Model.Update / Model.View dispatch (that stays the
// canonical flow); the registry is a surface any future caller (test harness,
// headless renderer, logging inspector) can walk to enumerate the TUI's screens
// without grepping a switch statement. Adding a new screen forces an entry here,
// so an unmapped constant can't slip past code review.

import tea "github.com/charmbracelet/bubbletea"

// screenHandler bundles the per-screen update + view methods. Both take a
// Model VALUE — bubbletea is immutable-by-convention — and the update returns
// (tea.Model, tea.Cmd) just like Update.
type screenHandler struct {
	update func(Model, tea.KeyMsg) (tea.Model, tea.Cmd)
	view   func(Model) string
}

// handlers is the screen → handler registry. Every screen constant in this
// package must appear exactly once; TestHandlersRegistry_AllScreensRegistered
// enumerates the constants and fails if any are missing.
var handlers = map[screen]screenHandler{
	screenList:          {update: Model.updateList, view: Model.viewList},
	screenSpawn:         {update: Model.updateSpawn, view: Model.viewSpawn},
	screenPickTemplate:  {update: Model.updatePickTemplate, view: Model.viewPickTemplate},
	screenForm:          {update: Model.updateForm, view: Model.viewForm},
	screenModelPick:     {update: Model.updateModelPick, view: Model.viewModelPick},
	screenRemoveConfirm: {update: Model.updateRemoveConfirm, view: Model.viewRemoveConfirm},
	screenResult:        {update: Model.updateResult, view: Model.viewResult},
	screenKeys:          {update: Model.updateKeys, view: Model.viewKeys},
	screenSetupTmux:     {update: Model.updateSetupTmux, view: Model.viewSetupTmux},
	screenSetup:         {update: Model.updateSetup, view: Model.viewSetup},
	screenCodexAuth:     {update: Model.updateCodexAuth, view: Model.viewCodexAuth},
}

// allScreens returns every screen constant defined in this package, in the
// order they're declared. Used by TestHandlersRegistry_AllScreensRegistered
// to assert the registry is exhaustive.
func allScreens() []screen {
	return []screen{
		screenList,
		screenSpawn,
		screenPickTemplate,
		screenForm,
		screenModelPick,
		screenRemoveConfirm,
		screenResult,
		screenKeys,
		screenSetupTmux,
		screenSetup,
		screenCodexAuth,
	}
}

// screenOwnedAsyncMsg is the marker interface for async messages tied to a
// specific screen — the picker's modelsMsg, the key manager's keysetMsg /
// keysSavedMsg / rotationSetMsg, etc. When the user has left the owning screen
// by the time the async result arrives, Update drops the message instead of
// mutating a Model field that no longer represents the active surface.
//
// An interface (rather than a struct field) lets existing async msgs opt into
// ownership incrementally; boardMsg / boardTickMsg already encode ownership via
// boardEpoch.
type screenOwnedAsyncMsg interface {
	owningScreen() screen
}
