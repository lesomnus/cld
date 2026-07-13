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
	"golang.org/x/term"
)

var lsHeaders = []string{"NAME", "ALIAS", "CONTAINER", "STATUS", "VERSION", "LOCAL FOLDER"}

func NewCmdLs() *xli.Command {
	return &xli.Command{
		Name:  "ls",
		Brief: "list devcontainers provisioned with claude",
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

			// On a terminal, render a colored, bordered table; when piped, fall
			// back to plain tab-separated columns so scripts can parse the output.
			if term.IsTerminal(int(os.Stdout.Fd())) {
				return renderLsTable(cmd, items, rows)
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

// renderLsTable draws the interactive, styled table: a subtle rounded border,
// an accented header, and the STATUS column colored by state. items supplies
// the per-row status used to color that column.
func renderLsTable(w io.Writer, items []daemon.Item, rows [][]string) error {
	const statusCol = 3
	t := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(tui.HelpStyle).
		Headers(lsHeaders...).
		Rows(rows...).
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
