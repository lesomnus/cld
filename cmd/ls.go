package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

var lsHeaders = []string{"NAME", "ALIAS", "CONTAINER", "STATUS", "VERSION", "LOCAL FOLDER", "ACTIVITY", "COST", "TOKENS", "RATE", "TITLE"}

func NewCmdLs() *xli.Command {
	return &xli.Command{
		Name:  "ls",
		Brief: "list devcontainers provisioned with claude",
		Flags: flg.Flags{
			&flg.Switch{Name: "wide", Alias: 'w', Brief: "show every column in plain, unstyled output"},
			&flg.Switch{Name: "debug-activity", Brief: "dump the raw captured pane behind each activity classification"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			if dbg, _ := flg.Get[bool](cmd, "debug-activity"); dbg {
				return renderActivityDebug(ctx, c.SocketPath())
			}

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
				// Activity is only meaningful for a ready container; a stopped or
				// session-ended one may still carry a last-pushed value in its
				// snapshot, so blank the column for non-ready rows (the card
				// renderer already gates activity on ready the same way).
				activity := ""
				if it.Status == daemon.StatusReady {
					activity = string(it.Activity)
				}
				cost, tokens, rate := telemetryCells(it.Telemetry)
				rows[i] = []string{displayName(it), it.Alias, id, string(it.Status), it.Version, abbreviate_home(it.LocalFolder), activity, cost, tokens, rate, it.Title}
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

// renderActivityDebug prints, for every ready container, the activity the
// daemon classified and the raw pane it captured to decide it. It exists to
// diagnose "always shows waiting, never working": the classifier keys entirely
// off whether the pane contains claude's interrupt hint, so seeing the actual
// captured text tells whether the pane came back empty (a capture problem) or
// simply lacked the expected hint string (a TUI-wording problem).
func renderActivityDebug(ctx context.Context, socket string) error {
	items, panes, err := daemon.FetchItemsDebug(ctx, socket)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Status != daemon.StatusReady {
			continue
		}
		pane := panes[it.ID]
		fmt.Printf("── %s (%s)\n", it.Alias, it.Name)
		fmt.Printf("   activity=%s  title=%q  pane_bytes=%d\n", it.Activity, it.Title, len(pane))
		if strings.TrimSpace(pane) == "" {
			fmt.Println("   [pane was EMPTY — capture returned nothing]")
		} else {
			fmt.Println("   ┄┄ captured pane ┄┄")
			for line := range strings.SplitSeq(strings.TrimRight(pane, "\n"), "\n") {
				fmt.Printf("   │ %s\n", line)
			}
		}
		fmt.Println()
	}
	return nil
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

// displayName is the user-facing label for a container: the collapsed Display
// when set, else the full managed Name. Older snapshots (and stopped entries
// resolved before Display existed) may carry no Display, so Name is the fallback.
func displayName(it daemon.Item) string {
	if it.Display != "" {
		return it.Display
	}
	return it.Name
}

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
		{displayName(it), cardNameStyle},
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
	// Consumption sits between the activity and the title, not after it: a
	// title is free-form and can run to the edge of the terminal, which would
	// push the numbers off screen exactly when a session is busy enough to be
	// worth watching.
	if tel := telemetryLabel(it.Telemetry); tel != "" {
		s += "  " + tel
	}
	if it.Title != "" {
		s += "  " + tui.HelpStyle.Render(it.Title)
	}
	return s
}

// telemetryCells renders a container's consumption as the three plain listing
// columns — cost, tokens, rate. All three are blank for a container that never
// reported (see daemon.Item.Telemetry), so a script can tell "not measured"
// from a measured zero.
func telemetryCells(t *daemon.Telemetry) (cost, tokens, rate string) {
	if t == nil {
		return "", "", ""
	}
	return formatCost(t.CostUSD), formatTokens(t.Tokens), formatRate(t.TokensPerMin)
}

// telemetryLabel is the card form of the same three figures, joined by middots
// and dimmed so they read as an annotation on the activity rather than
// competing with it. Empty when the container never reported; the rate is
// dropped once it falls to zero, since a resting session showing a "~0" is
// noise the activity word already conveys.
//
// Unlike the column layouts, a card has no header to carry the rate's unit —
// and the total and the rate are both token counts, so without it the two read
// as the same measurement twice. The "/m" is therefore appended HERE rather
// than baked into formatRate (see rateUnit).
func telemetryLabel(t *daemon.Telemetry) string {
	if t == nil {
		return ""
	}
	parts := []string{formatCost(t.CostUSD), formatTokens(t.Tokens)}
	if r := formatRate(t.TokensPerMin); r != "" {
		parts = append(parts, r+rateUnit)
	}
	return tui.HelpStyle.Render(strings.Join(parts, " · "))
}

// rateUnit spells the rate's denominator for layouts with no column header to
// carry it. The tabular views put it in the header instead (see the ls RATE
// column and watchRateHeader), keeping the cells themselves as narrow as the
// numbers they hold.
const rateUnit = "/m"

// formatCost renders claude's own cost estimate. Always two decimals: the
// figure is an estimate (and notional on a subscription plan), so a stable,
// unrounded-looking form is less likely to be read as a billed amount than a
// number that changes shape as it grows.
func formatCost(usd float64) string {
	return fmt.Sprintf("$%.2f", usd)
}

// formatTokens abbreviates a token count to three significant figures at most —
// 938, 12.3k, 1.2M — so the column stays narrow and comparable down the list.
func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.FormatInt(n, 10)
	}
}

// formatRate renders recent consumption as a bare token count, prefixed "~"
// because it is an average over a trailing window rather than an instantaneous
// reading. The per-minute denominator is NOT included: it belongs in the column
// header, so the cells stay as narrow as their numbers. A layout with no header
// appends rateUnit itself. Empty at zero so a quiet session shows nothing.
func formatRate(perMin float64) string {
	if perMin < 1 {
		return ""
	}
	return "~" + formatTokens(int64(perMin))
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
