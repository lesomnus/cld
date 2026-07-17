package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

var lsHeaders = []string{"NAME", "ALIAS", "CONTAINER", "STATUS", "VERSION", "LOCAL FOLDER", "ACTIVITY", "TITLE"}

func NewCmdLs() *xli.Command {
	return &xli.Command{
		Name:  "ls",
		Brief: "list devcontainers provisioned with claude",
		Flags: flg.Flags{
			&flg.Switch{Name: "wide", Alias: 'w', Brief: "show every column in plain, unstyled output"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			items, err := daemon.FetchItems(ctx, c.SocketPath())
			if err != nil {
				return err
			}

			rows := make([][]string, len(items))
			for i, it := range items {
				id := it.ID
				if len(id) > 12 {
					id = id[:12]
				}
				rows[i] = []string{it.Name, it.Alias, id, string(it.Status), it.Version, abbreviate_home(it.LocalFolder), string(it.Activity), it.Title}
			}

			// --wide always prints every column as plain tab-separated text, no
			// border or color — a stable, complete view regardless of width.
			if wide, _ := flg.Get[bool](cmd, "wide"); wide {
				return renderLsPlain(cmd, rows)
			}

			// On a terminal, render styled cards — two lines per container, the
			// left curve colored by lifecycle status and the second line showing
			// the live conversation. When piped, fall back to plain tab-separated
			// columns so scripts keep parsing stable columns.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				return renderLsCards(cmd, items)
			}
			return renderLsPlain(cmd, rows)
		}),
	}
}

// renderLsPlain writes the classic tab-aligned listing, used when stdout is not
// a terminal so downstream tools keep parsing stable columns.
func renderLsPlain(w io.Writer, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(lsHeaders, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	return tw.Flush()
}

var (
	// cardAliasStyle renders a card's alias — the leading, brightest field,
	// since the alias is the handle used most often.
	cardAliasStyle = tui.TitleStyle
	// cardNameStyle renders the full container name at normal weight: readable
	// but quieter than the alias.
	cardNameStyle = lipgloss.NewStyle()
	// cardWorkingStyle accents the "working" activity so an active conversation
	// stands out at a glance; the quieter states stay dim.
	cardWorkingStyle = tui.TitleStyle
)

// cardIdentityCells returns a card's first-line fields in display order —
// alias, name, container, version, folder — each with its own style. The order
// leads with the alias; widths are equalized across cards by renderLsCards.
func cardIdentityCells(it daemon.Item) []struct {
	text  string
	style lipgloss.Style
} {
	id := it.ID
	if len(id) > 12 {
		id = id[:12]
	}
	return []struct {
		text  string
		style lipgloss.Style
	}{
		{it.Alias, cardAliasStyle},
		{it.Name, cardNameStyle},
		{id, tui.HelpStyle},
		{it.Version, tui.HelpStyle},
		{abbreviate_home(it.LocalFolder), tui.HelpStyle},
	}
}

// renderLsCards draws one two-line card per container. A left curve (╭ over ╰),
// colored by lifecycle status, brackets each card so adjacent cards separate
// without a blank line between them. The first line is the identity, laid out
// in fixed-width columns (alias, name, container, version, folder) so the
// fields line up down the list regardless of name length; the second is the
// live conversation — activity and title for a ready container, or the
// lifecycle state otherwise.
func renderLsCards(w io.Writer, items []daemon.Item) error {
	// Each identity column is padded to the widest cell across all cards, so a
	// short name never shifts the columns after it out of alignment. A column
	// no card fills (e.g. no aliases at all) collapses to nothing.
	ncol := len(cardIdentityCells(daemon.Item{}))
	widths := make([]int, ncol)
	last := -1 // highest-indexed column any card fills; it is not padded
	for _, it := range items {
		for i, c := range cardIdentityCells(it) {
			if wd := lipgloss.Width(c.text); wd > widths[i] {
				widths[i] = wd
			}
		}
	}
	for i, wd := range widths {
		if wd > 0 {
			last = i
		}
	}

	var b strings.Builder
	for _, it := range items {
		curve := tui.StatusStyle(string(it.Status))
		parts := make([]string, 0, ncol)
		for i, c := range cardIdentityCells(it) {
			if widths[i] == 0 {
				continue
			}
			// Pad every column to its width except the last one, whose trailing
			// padding would just be invisible whitespace at the line's end.
			if i == last {
				parts = append(parts, c.style.Render(c.text))
			} else {
				parts = append(parts, c.style.Width(widths[i]).Render(c.text))
			}
		}
		fmt.Fprintf(&b, "%s %s\n", curve.Render("╭"), strings.Join(parts, "  "))
		fmt.Fprintf(&b, "%s %s\n", curve.Render("╰"), cardState(it))
	}
	_, err := fmt.Fprint(w, b.String())
	return err
}

// cardState is a card's second line. For a ready container it is the live
// conversation — an activity icon and word, then claude's title; a failed
// container shows its error, and any other state shows the lifecycle word.
func cardState(it daemon.Item) string {
	if it.Status != daemon.StatusReady {
		if it.Status == daemon.StatusFailed && it.Error != "" {
			return tui.StatusStyle(string(it.Status)).Render(it.Error)
		}
		return tui.HelpStyle.Render(string(it.Status))
	}

	icon, style := activityLook(it.Activity)
	s := style.Render(icon + " " + string(it.Activity))
	if it.Title != "" {
		s += "  " + tui.HelpStyle.Render(it.Title)
	}
	return s
}

// activityLook maps a conversation activity to its bullet and style: a bright
// spinner glyph for working, quiet bullets for the idle states.
func activityLook(a daemon.Activity) (string, lipgloss.Style) {
	switch a {
	case daemon.ActivityWorking:
		return "⟳", cardWorkingStyle
	case daemon.ActivityIdle:
		return "○", tui.HelpStyle
	default: // waiting, or an unknown/empty value on a ready container
		return "◦", tui.HelpStyle
	}
}

// abbreviate_home shortens a path under this client's home directory to a
// leading "~". The local folder is a host path, so this only fires when the
// client shares that home (running on the host); run inside a container with a
// different home it leaves the full path, never mis-abbreviating it.
func abbreviate_home(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return abbreviate_home_in(p, home)
}

func abbreviate_home_in(p, home string) string {
	if p == "" || home == "" || home == "/" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+"/"); ok {
		return "~/" + rest
	}
	return p
}
