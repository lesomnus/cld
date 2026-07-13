// Package tui holds cld's small interactive terminal widgets, built on
// bubbletea/lipgloss. Everything here is a thin, self-contained helper — a
// single-choice picker and a yes/no confirm — plus the shared palette the rest
// of the CLI styles its output with. The widgets render on stderr and refuse to
// run without a real terminal, so stdout stays clean for pipes and the caller
// can fall back to a plain path when ErrNotInteractive comes back.
package tui

import (
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

// ErrNotInteractive is returned by the widgets when stdin or stderr is not a
// terminal, so callers can fall back to a non-interactive path.
var ErrNotInteractive = errors.New("no interactive terminal")

// ErrAborted is returned when the user cancels a widget (Ctrl-C, Esc, or q).
var ErrAborted = errors.New("aborted")

// Interactive reports whether both stdin and stderr are terminals, i.e. whether
// the interactive widgets can run.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// run drives a model on the real terminal, rendering to stderr so stdout stays
// free for machine-readable output. It refuses to start without a TTY.
func run(m tea.Model) (tea.Model, error) {
	if !Interactive() {
		return nil, ErrNotInteractive
	}
	p := tea.NewProgram(m, tea.WithOutput(os.Stderr), tea.WithInput(os.Stdin))
	return p.Run()
}
