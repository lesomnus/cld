package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Choice is one selectable row: a Title and an optional dim Desc shown after it.
type Choice struct {
	Title string
	Desc  string
}

// selectModel is the bubbletea model backing Select. It is exported-package
// internal so its Update logic can be unit-tested without a terminal.
type selectModel struct {
	title  string
	items  []Choice
	cursor int
	chosen int  // index of the picked item, or -1 while running
	abort  bool // set when the user cancels
}

func newSelectModel(title string, items []Choice) selectModel {
	return selectModel{title: title, items: items, chosen: -1}
}

func (m selectModel) Init() tea.Cmd { return nil }

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j", "ctrl+n":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.items) - 1
	case "enter":
		m.chosen = m.cursor
		return m, tea.Quit
	case "ctrl+c", "esc", "q":
		m.abort = true
		return m, tea.Quit
	}
	return m, nil
}

func (m selectModel) View() string {
	// Once a choice is made or the widget is cancelled, render nothing: the
	// caller prints its own resolved line, keeping the scrollback tidy.
	if m.chosen >= 0 || m.abort {
		return ""
	}

	var b strings.Builder
	b.WriteString(TitleStyle.Render(m.title))
	b.WriteByte('\n')
	for i, it := range m.items {
		line := it.Title
		if it.Desc != "" {
			line += "  " + DescStyle.Render(it.Desc)
		}
		if i == m.cursor {
			b.WriteString(SelectedStyle.Render("› " + line))
		} else {
			b.WriteString(ItemStyle.Render(line))
		}
		b.WriteByte('\n')
	}
	b.WriteString(HelpStyle.Render("↑/↓ move · enter select · esc cancel"))
	b.WriteByte('\n')
	return b.String()
}

// Select shows an interactive single-choice list titled title and returns the
// index of the chosen item. It returns ErrAborted if the user cancels and
// ErrNotInteractive when there is no terminal to draw on. With a single item it
// still prompts, so the caller always gets an explicit choice.
func Select(title string, items []Choice) (int, error) {
	if len(items) == 0 {
		return -1, ErrAborted
	}
	res, err := run(newSelectModel(title, items))
	if err != nil {
		return -1, err
	}
	m := res.(selectModel)
	if m.abort {
		return -1, ErrAborted
	}
	return m.chosen, nil
}
