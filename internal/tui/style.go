package tui

import (
	"math"
	"strconv"

	"github.com/charmbracelet/lipgloss"
)

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

// GaugeStyle colors a utilization percentage (0–100): green while there is
// plenty of headroom, yellow past the halfway mark, red as it nears the cap.
func GaugeStyle(pct float64) lipgloss.Style {
	s := lipgloss.NewStyle()
	switch {
	case pct >= 80:
		return s.Foreground(red)
	case pct >= 50:
		return s.Foreground(yellow)
	default:
		return s.Foreground(green)
	}
}

// RateStyle brightens a per-minute token rate: a quiet session's figure sits
// dim, a fast-generating one glows near-white, so the eye is drawn down the
// column to whoever is burning tokens right now. The ramp is the ANSI grayscale
// block (240 → 255) walked on a LOG scale, because token rates span orders of
// magnitude — a few hundred a minute to tens of thousands — and a linear map
// would leave everything but the very top pinned at the dim end.
func RateStyle(perMin float64) lipgloss.Style {
	s := lipgloss.NewStyle()
	if perMin < 1 {
		return s.Foreground(subtle)
	}
	// log10(perMin) is 0 at 1/min and 4 at 10k/min; map that span onto the 15
	// grayscale steps from 240 (dim) to 255 (bright), clamped at both ends.
	lv := math.Log10(perMin) / 4
	lv = math.Max(0, math.Min(1, lv))
	idx := 240 + int(math.Round(lv*15))
	return s.Foreground(lipgloss.Color(strconv.Itoa(idx)))
}

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
