package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// confirmModel backs Confirm: a yes/no toggle defaulting to no.
type confirmModel struct {
	prompt string
	yes    bool
	done   bool
	abort  bool
}

func (m confirmModel) Init() tea.Cmd { return nil }

func (m confirmModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "left", "right", "tab", "h", "l":
		m.yes = !m.yes
	case "y", "Y":
		m.yes, m.done = true, true
		return m, tea.Quit
	case "n", "N":
		m.yes, m.done = false, true
		return m, tea.Quit
	case "enter":
		m.done = true
		return m, tea.Quit
	case "ctrl+c", "esc":
		m.abort = true
		return m, tea.Quit
	}
	return m, nil
}

func (m confirmModel) View() string {
	if m.done || m.abort {
		return ""
	}
	yes, no := "Yes", "No"
	if m.yes {
		yes = SelectedStyle.PaddingLeft(0).Render("[Yes]")
		no = ItemStyle.PaddingLeft(0).Render("No")
	} else {
		yes = ItemStyle.PaddingLeft(0).Render("Yes")
		no = SelectedStyle.PaddingLeft(0).Render("[No]")
	}
	var b strings.Builder
	b.WriteString(TitleStyle.Render(m.prompt))
	b.WriteString("  ")
	b.WriteString(yes)
	b.WriteString("  ")
	b.WriteString(no)
	b.WriteByte('\n')
	b.WriteString(HelpStyle.Render("←/→ move · y/n · enter confirm · esc cancel"))
	b.WriteByte('\n')
	return b.String()
}

// Confirm asks a yes/no question, defaulting to no, and returns the answer. It
// returns ErrAborted if the user cancels and ErrNotInteractive without a
// terminal, letting callers decide the safe default in each case.
func Confirm(prompt string) (bool, error) {
	res, err := run(confirmModel{prompt: prompt})
	if err != nil {
		return false, err
	}
	m := res.(confirmModel)
	if m.abort {
		return false, ErrAborted
	}
	return m.yes, nil
}
