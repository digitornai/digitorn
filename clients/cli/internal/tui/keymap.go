// Package tui builds the Bubble Tea fullscreen interface. Each widget
// is its own file ; the root Model in app.go owns screen routing,
// resize, and global keys. Widgets export a tea.Model-shaped API
// (Update / View) so they can be composed.
package tui

import "charm.land/bubbles/v2/key"

// Keymap collects every key binding in one place. Widgets reference
// it instead of inlining string keys, so a theme/customization layer
// (CLI-8) can rebind everything by mutating a single struct.
type Keymap struct {
	// Global.
	Quit     key.Binding
	Help     key.Binding
	Commands key.Binding
	Sessions key.Binding
	Apps     key.Binding
	NewTab   key.Binding
	CloseTab key.Binding
	NextTab  key.Binding
	PrevTab  key.Binding

	// Chat input.
	SubmitMessage key.Binding
	InsertNewline key.Binding
	ClearInput    key.Binding

	// Viewport.
	ScrollUp   key.Binding
	ScrollDown key.Binding
	GoToTop    key.Binding
	GoToBottom key.Binding
}

// DefaultKeymap is what new TUI sessions get out of the box. Mirrors
// opencode + Claude Code conventions where reasonable so muscle memory
// transfers.
func DefaultKeymap() Keymap {
	return Keymap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+q", "ctrl+c"),
			key.WithHelp("ctrl+q", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
		Commands: key.NewBinding(
			key.WithKeys("ctrl+k"),
			key.WithHelp("ctrl+k", "commands"),
		),
		Sessions: key.NewBinding(
			key.WithKeys("ctrl+s"),
			key.WithHelp("ctrl+s", "sessions"),
		),
		Apps: key.NewBinding(
			key.WithKeys("ctrl+a"),
			key.WithHelp("ctrl+a", "apps"),
		),
		NewTab: key.NewBinding(
			key.WithKeys("ctrl+t"),
			key.WithHelp("ctrl+t", "new tab"),
		),
		CloseTab: key.NewBinding(
			key.WithKeys("ctrl+w"),
			key.WithHelp("ctrl+w", "close tab"),
		),
		NextTab: key.NewBinding(
			key.WithKeys("ctrl+tab"),
			key.WithHelp("ctrl+tab", "next tab"),
		),
		PrevTab: key.NewBinding(
			key.WithKeys("ctrl+shift+tab"),
			key.WithHelp("ctrl+shift+tab", "prev tab"),
		),
		SubmitMessage: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "send"),
		),
		InsertNewline: key.NewBinding(
			key.WithKeys("shift+enter"),
			key.WithHelp("shift+enter", "newline"),
		),
		ClearInput: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "clear input"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdn", "scroll down"),
		),
		GoToTop: key.NewBinding(
			key.WithKeys("home"),
			key.WithHelp("home", "top"),
		),
		GoToBottom: key.NewBinding(
			key.WithKeys("end"),
			key.WithHelp("end", "bottom"),
		),
	}
}
