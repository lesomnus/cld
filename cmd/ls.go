package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

var lsHeaders = []string{"NAME", "ALIAS", "CONTAINER", "STATUS", "VERSION", "LOCAL FOLDER"}

// lsDropOrder lists column indices from least to most important. When the
// terminal is too narrow to hold the whole table, columns are dropped in this
// order until it fits. NAME (0) is never dropped. Importance, most first, is
// NAME > CONTAINER > ALIAS > STATUS > LOCAL FOLDER > VERSION.
var lsDropOrder = []int{4, 5, 3, 1, 2}

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
				rows[i] = []string{it.Name, it.Alias, id, string(it.Status), it.Version, abbreviate_home(it.LocalFolder)}
			}

			// --wide always prints every column as plain tab-separated text, no
			// border or color — a stable, complete view regardless of width.
			if wide, _ := flg.Get[bool](cmd, "wide"); wide {
				return renderLsPlain(cmd, rows)
			}

			// On a terminal, render a colored, bordered table, hiding the
			// lowest-priority columns when it would otherwise overflow the
			// width; when piped, fall back to plain tab-separated columns so
			// scripts keep parsing stable columns.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				width, _, err := term.GetSize(int(os.Stdout.Fd()))
				if err != nil || width <= 0 {
					width = 1 << 30 // unknown width: assume room for everything
				}
				return renderLsTable(cmd, items, rows, lsFitColumns(rows, width))
			}
			return renderLsPlain(cmd, rows)
		}),
	}
}

// lsFitColumns returns the display column indices, in ascending order, that fit
// within maxWidth. It starts from every column and drops them one at a time in
// lsDropOrder (least important first) until the rendered table fits or only
// NAME remains.
func lsFitColumns(rows [][]string, maxWidth int) []int {
	hidden := make([]bool, len(lsHeaders))
	for {
		cols := lsVisibleColumns(hidden)
		if lsTableWidth(rows, cols) <= maxWidth {
			return cols
		}
		dropped := false
		for _, c := range lsDropOrder {
			if !hidden[c] {
				hidden[c] = true
				dropped = true
				break
			}
		}
		if !dropped {
			return lsVisibleColumns(hidden) // nothing left to drop but NAME
		}
	}
}

func lsVisibleColumns(hidden []bool) []int {
	cols := make([]int, 0, len(hidden))
	for i, h := range hidden {
		if !h {
			cols = append(cols, i)
		}
	}
	return cols
}

// lsTableWidth is the on-screen width of the bordered table for the given
// visible columns: one vertical rule between and around each column, plus each
// column's widest cell padded by one cell on each side (matching the Padding
// used in renderLsTable).
func lsTableWidth(rows [][]string, cols []int) int {
	w := len(cols) + 1 // vertical borders: one per column plus a trailing edge
	for _, c := range cols {
		cw := lipgloss.Width(lsHeaders[c])
		for _, r := range rows {
			if x := lipgloss.Width(r[c]); x > cw {
				cw = x
			}
		}
		w += cw + 2 // left+right padding of 1
	}
	return w
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

// renderLsTable draws the interactive, styled table: a subtle rounded border,
// an accented header, and the STATUS column colored by state. items supplies
// the per-row status used to color that column. cols selects which display
// columns to show, in ascending order, so narrow terminals can drop the
// lowest-priority ones.
func renderLsTable(w io.Writer, items []daemon.Item, rows [][]string, cols []int) error {
	// The STATUS column may shift left once earlier columns are hidden, so
	// find its position in the visible set to color the right cell.
	const statusSrc = 3
	statusCol := -1
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = lsHeaders[c]
		if c == statusSrc {
			statusCol = i
		}
	}
	shown := make([][]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(cols))
		for j, c := range cols {
			cells[j] = r[c]
		}
		shown[i] = cells
	}

	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(tui.HelpStyle).
		Headers(headers...).
		Rows(shown...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return tui.TitleStyle.Padding(0, 1)
			}
			if col == statusCol && row < len(items) {
				return tui.StatusStyle(string(items[row].Status)).Padding(0, 1)
			}
			return lipgloss.NewStyle().Padding(0, 1)
		})
	_, err := fmt.Fprintln(w, t)
	return err
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
