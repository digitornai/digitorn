package tui

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// SelectApp runs a standalone fullscreen picker over the installed apps
// and returns the chosen app_id. An empty string means the user cancelled
// (esc / ctrl+c). Used by `digitorn chat` with no app argument.
func SelectApp(apps []client.App, th *theme.Theme) (string, error) {
	items := make([]PickerItem, 0, len(apps))
	for _, a := range apps {
		hint := a.AppID
		if a.Version != "" {
			hint += " · " + a.Version
		}
		items = append(items, PickerItem{ID: a.AppID, Label: a.Name, Hint: hint})
	}
	m := &appSelector{picker: NewPicker(th, "select an app to chat with", items), theme: th}
	res, err := tea.NewProgram(m).Run()
	if err != nil {
		return "", err
	}
	if fm, ok := res.(*appSelector); ok {
		return fm.choice, nil
	}
	return "", nil
}

type appSelector struct {
	picker *Picker
	theme  *theme.Theme
	choice string
	w, h   int
}

func (m *appSelector) Init() tea.Cmd { return nil }

func (m *appSelector) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.picker.SetSize(msg.Width, msg.Height)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		a := m.picker.Update(msg)
		if a.Cancelled {
			return m, tea.Quit
		}
		if a.Selected != "" {
			m.choice = a.Selected
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m *appSelector) View() tea.View {
	v := tea.NewView("")
	v.AltScreen = true
	if m.w == 0 || m.h == 0 {
		return v
	}
	m.picker.SetSize(m.w, m.h)
	v.Content = lipgloss.NewStyle().
		Width(m.w).
		Height(m.h).
		Background(lipgloss.Color(m.theme.Background)).
		Foreground(lipgloss.Color(m.theme.Text)).
		Render(m.picker.View())
	return v
}
