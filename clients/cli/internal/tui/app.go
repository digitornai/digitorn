package tui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

// Screen is the enum of top-level views. CLI-3 renders Chat ; CLI-5
// adds Sessions, CLI-7 adds Apps.
type Screen int

const (
	ScreenChat Screen = iota
	ScreenSessions
	ScreenApps
)

// Model is the root Bubble Tea state. It owns global concerns
// (size, theme, keymap, active screen, status bar) and delegates
// content rendering to a screen-specific sub-model.
type Model struct {
	// dependencies (immutable after New)
	client *client.Client
	theme  *theme.Theme
	keymap Keymap
	appID  string
	creds  *client.Credentials

	// terminal geometry, refreshed on every WindowSizeMsg
	width  int
	height int

	// global state
	screen    Screen
	statusBar *StatusBar
	chat      *ChatScreen

	// transient
	err error
}

// Options is the construction-time bundle. The chat subcommand
// validates and fills this before calling New.
type Options struct {
	Client      *client.Client
	Theme       *theme.Theme
	AppID       string
	AppName     string
	Credentials *client.Credentials
	// ResumeSessionID is set by `digitorn chat <app> --session <sid>` to
	// re-open an existing session instead of creating a fresh one.
	// Empty = fresh session. The runtime replays history via Socket.IO
	// once we JoinSession.
	ResumeSessionID string
	// Workdir is the agent's working directory for a fresh session — by
	// default the directory the CLI was launched from. Sent on session
	// creation ; the daemon uses an absolute, existing dir as-is (file tools
	// then operate there). Empty = let the daemon pick a managed dir.
	Workdir string
}

// New builds the root Model. The TUI is started via Run() ; nothing
// is rendered until the first Update tick from the program.
func New(opts Options) *Model {
	th := opts.Theme
	if th == nil {
		th = theme.Default()
	}
	statusBar := NewStatusBar(th)
	statusBar.AppName = opts.AppName
	if opts.AppName == "" {
		statusBar.AppName = opts.AppID
	}
	statusBar.Conn = ConnConnecting
	if opts.Credentials != nil {
		statusBar.UserEmail = opts.Credentials.Email
	}
	m := &Model{
		client:    opts.Client,
		theme:     th,
		keymap:    DefaultKeymap(),
		appID:     opts.AppID,
		creds:     opts.Credentials,
		screen:    ScreenChat,
		statusBar: statusBar,
	}
	m.chat = NewChatScreen(opts.Client, opts.AppID, m)
	m.chat.resumeSessionID = opts.ResumeSessionID
	m.chat.reqWorkdir = opts.Workdir
	return m
}

// Init is the Bubble Tea entry. We probe the daemon AND bootstrap a
// chat session in parallel — both fire their own messages, the
// Update routes them.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.probeConn(), m.chat.Bootstrap(), m.chat.FetchModel(), m.chat.FetchModes(), m.chat.FetchAppInfo())
}

// Update is the global event router. Order :
//  1. Global keys (quit) — eaten before anything else can react
//  2. Window resize — propagated to statusbar + active screen
//  3. Custom messages (conn state) — update statusbar
//  4. Everything else — delegated to the active screen
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.statusBar.SetWidth(msg.Width)
		// Size the chat to fill everything above the statusbar, using the SAME
		// measured statusbar height the View uses — a hardcoded "-1" would leave
		// the sidebar border a row short (or long) of the statusbar if the bar
		// ever wrapped to a second line.
		sbH := lipgloss.Height(m.statusBar.View())
		if sbH < 1 {
			sbH = 1
		}
		bodyH := msg.Height - sbH
		if bodyH < 1 {
			bodyH = 1
		}
		m.chat.SetSize(msg.Width, bodyH)
		return m, nil

	case tea.KeyMsg:
		if key.Matches(msg, m.keymap.Quit) {
			return m, tea.Quit
		}
		// Command palette : a fuzzy list of every command, opencode style.
		// The chat screen returns nil while a modal owns the keyboard, so
		// the key falls through to normal routing in that case.
		if key.Matches(msg, m.keymap.Commands) && m.screen == ScreenChat {
			if cmd := m.chat.OpenCommandPalette(); cmd != nil {
				return m, cmd
			}
		}
		if m.screen == ScreenChat {
			switch {
			case key.Matches(msg, m.keymap.Sessions):
				if cmd := m.chat.OpenPicker("sessions"); cmd != nil {
					return m, cmd
				}
			case key.Matches(msg, m.keymap.Apps):
				if cmd := m.chat.OpenPicker("apps"); cmd != nil {
					return m, cmd
				}
			}
		}
		// Fall through to screen routing.

	case connStatusMsg:
		m.statusBar.Conn = msg.state
		if msg.err != nil {
			m.err = msg.err
		}
		return m, nil
	}

	// Delegate everything else to the active screen.
	var cmd tea.Cmd
	switch m.screen {
	case ScreenChat:
		cmd = m.chat.Update(msg)
		// Refresh statusbar from screen state every tick (cheap).
		// The chat screen owns appID/appName since /apps may swap them.
		m.statusBar.AppName = m.chat.AppName()
		m.statusBar.Session = m.chat.SessionID()
		m.statusBar.Model = m.chat.Model()
		// Conn dot reflects the realtime stream — REST health is irrelevant
		// once we're past the initial probe.
		switch m.chat.ConnState() {
		case client.ConnStateConnected:
			m.statusBar.Conn = ConnConnected
		case client.ConnStateReconnecting:
			m.statusBar.Conn = ConnReconnecting
		case client.ConnStateDisconnected:
			m.statusBar.Conn = ConnDisconnected
		default:
			m.statusBar.Conn = ConnConnecting
		}
	}
	return m, cmd
}

// View renders the current frame.
//
//	[screen body  ...                                ]
//	[statusbar                                       ]
func (m *Model) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	// Mouse ON (click to expand chips, wheel to scroll). CellMotion is the
	// minimal mode that still delivers clicks + wheel. If mouse-report garbage
	// ever leaks to the shell, it's an abnormal exit leaving the terminal in
	// mouse mode — covered by the disableMouseTracking() defer on exit and a
	// clean Ctrl+C quit. Keyboard works too (Ctrl+O expand all, PgUp/PgDn
	// scroll, Ctrl+P/Ctrl+N select + Enter).
	view.MouseMode = tea.MouseModeCellMotion
	if m.width == 0 {
		return view
	}
	statusbar := m.statusBar.View()
	statusbarHeight := lipgloss.Height(statusbar)

	bodyHeight := m.height - statusbarHeight
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	var body string
	switch m.screen {
	case ScreenChat:
		body = m.chat.View()
	default:
		body = m.placeholderBody()
	}

	// Size the body frame but DON'T paint a background : empty regions (e.g. the
	// working-indicator line) show the bare terminal background instead of the
	// theme's near-black. Panels that want a fill (chips, message bands) set
	// their own background explicitly, so they're unaffected.
	framed := lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		Foreground(lipgloss.Color(m.theme.Text)).
		Render(body)

	view.Content = lipgloss.JoinVertical(lipgloss.Left, framed, statusbar)
	return view
}

// connStatusMsg is fired by probeConn (and later by the reconnect
// loop in CLI-9) to update the connection dot.
type connStatusMsg struct {
	state ConnState
	err   error
}

// probeConn pings /health in a tea.Cmd so the first frame shows a
// realistic dot instead of a permanent "connecting" badge.
func (m *Model) probeConn() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.client.Ping(ctx); err != nil {
			return connStatusMsg{state: ConnDisconnected, err: err}
		}
		return connStatusMsg{state: ConnConnected}
	}
}

// placeholderBody is shown for screens not yet implemented. CLI-5
// fills Sessions, CLI-7 fills Apps.
func (m *Model) placeholderBody() string {
	hint := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.TextMuted)).
		Padding(2, 4).
		Render("(screen not yet implemented — press Ctrl+Q to quit)")
	return hint
}
