package tui

import "github.com/charmbracelet/lipgloss"

// The shared palette. cld's accent is a warm pink; secondary text is dimmed.
// Colors are given as ANSI 256 indices so they degrade gracefully on limited
// terminals, and lipgloss drops them entirely when the output is not a TTY.
var (
	accent = lipgloss.Color("205") // pink
	subtle = lipgloss.Color("240") // dim gray
	green  = lipgloss.Color("42")
	red    = lipgloss.Color("203")
	yellow = lipgloss.Color("214")
)

var (
	// TitleStyle renders a widget's heading.
	TitleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	// HelpStyle renders the dim key-hint footer under a widget.
	HelpStyle = lipgloss.NewStyle().Foreground(subtle)
	// ItemStyle renders an unselected list row.
	ItemStyle = lipgloss.NewStyle().PaddingLeft(2)
	// SelectedStyle renders the highlighted list row.
	SelectedStyle = lipgloss.NewStyle().PaddingLeft(0).Foreground(accent).Bold(true)
	// DescStyle renders a list row's secondary description text.
	DescStyle = lipgloss.NewStyle().Foreground(subtle)
)

// tag is the styled "cld" badge printed ahead of status lines.
var tagStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)

// Tag returns the styled "cld" prefix (without a trailing space) used to open
// cld's human-facing status lines. It matches the "cld:" convention already in
// the codebase while giving it a little color on a terminal.
func Tag() string { return tagStyle.Render("cld") }

// StatusStyle maps a devcontainer status word to a color for list rendering.
func StatusStyle(status string) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch status {
	case "ready":
		return s.Foreground(green)
	case "failed":
		return s.Foreground(red)
	case "provisioning", "session-ended":
		return s.Foreground(yellow)
	default:
		return s
	}
}
