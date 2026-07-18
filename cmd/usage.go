package cmd

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/cld/internal/usage"
	"github.com/lesomnus/xli"
)

func NewCmdUsage() *xli.Command {
	return &xli.Command{
		Name:  "usage",
		Brief: "show Claude subscription usage for every login cld can see",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			report, err := daemon.FetchUsage(ctx, c.SocketPath())
			if err != nil {
				return err
			}
			return renderUsage(os.Stdout, report)
		}),
	}
}

// renderUsage prints one aligned row per login: its label, the 5-hour window,
// and the weekly window (or an error in place of both). A login with no source
// at all prints a single hint line so the empty case is not a blank screen.
func renderUsage(w io.Writer, report *daemon.UsageReport) error {
	if report == nil || len(report.Sources) == 0 {
		fmt.Fprintln(w, tui.HelpStyle.Render("no login to report usage for — run `cld auth login` or start a session"))
		return nil
	}

	tw := tabwriter.NewWriter(w, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, tui.HelpStyle.Render("LOGIN\t5-HOUR\tWEEKLY"))
	for _, s := range report.Sources {
		if s.Error != "" {
			fmt.Fprintf(tw, "%s\t%s\n", s.Label, tui.StatusStyle("failed").Render("⚠ "+s.Error))
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n",
			s.Label,
			usageWindow(s.Usage.FiveHour, false),
			usageWindow(s.Usage.SevenDay, true),
		)
	}
	return tw.Flush()
}

// usageWindow formats one rate-limit window as "<pct>% · resets <when>", the
// percentage colored by how close it is to the cap. weekly widens the reset
// clock to include the weekday. A zero reset time (endpoint reported null)
// drops the reset clause.
func usageWindow(win usage.Window, weekly bool) string {
	pct := tui.GaugeStyle(win.Utilization).Render(fmt.Sprintf("%.0f%%", win.Utilization))
	when := formatReset(win.ResetsAt, weekly)
	if when == "" {
		return pct
	}
	return pct + tui.HelpStyle.Render(" · resets "+when)
}

// formatReset renders a reset instant in local time: a bare clock for the
// short 5-hour window, a weekday-prefixed clock for the weekly one (which can
// land days out). A zero time yields "".
func formatReset(t time.Time, weekly bool) string {
	if t.IsZero() {
		return ""
	}
	if weekly {
		return t.Local().Format("Mon 15:04")
	}
	return t.Local().Format("15:04")
}

// usageGaugeTrack is the dim baseline rail drawn under the unfilled part of the
// bar (bottom dot-row only), so the gauge's full width is always visible.
const usageGaugeTrack = '⣀'

// usageGaugeRise are the braille glyphs for a cell filled 1–5 dots ABOVE the
// always-on baseline (dots 7+8): the left column bottom-up (⣄⣆⣇), then the
// right column bottom-up (⣧⣷). Every glyph — and the track — keeps both bottom
// dots lit, so the baseline reads as a continuous rail while the leading cell
// paints on one dot at a time, left to right. 6 sub-steps per cell (the 6th
// fills the cell to ⣿), 6×cells resolution.
var usageGaugeRise = [5]string{"⣄", "⣆", "⣇", "⣧", "⣷"}

// usageGauge renders pct (0–100) as a braille bar `cells` wide: full cells are
// solid (⣿), the single leading cell fills dot-by-dot from the left on top of
// the baseline, and the rest is the dim baseline track. The filled part is
// colored by load.
func usageGauge(pct float64, cells int) string {
	if cells < 1 {
		cells = 1
	}
	p := math.Max(0, math.Min(100, pct))
	units := int(math.Round(p / 100 * float64(cells*6))) // dots above the baseline
	full, rem := units/6, units%6

	var filled strings.Builder
	filled.WriteString(strings.Repeat("⣿", full))
	track := cells - full
	if rem > 0 {
		filled.WriteString(usageGaugeRise[rem-1])
		track--
	}
	return tui.GaugeStyle(pct).Render(filled.String()) +
		tui.HelpStyle.Render(strings.Repeat(string(usageGaugeTrack), track))
}

// usageRemaining formats the time left until a window resets, compactly: minutes
// under an hour, h+m under a day, d+h beyond. Empty when the reset time is
// unknown; "0m" once it is due.
func usageRemaining(reset, now time.Time) string {
	if reset.IsZero() {
		return ""
	}
	d := reset.Sub(now)
	switch {
	case d <= 0:
		return "0m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// usageBar is one source's bottom-of-screen line for `cld watch`: the 5-hour and
// weekly gauges, each with its percentage and time-to-reset, and no account
// name. Right-alignment is the caller's job.
func usageBar(s daemon.UsageSource, now time.Time) string {
	if s.Error != "" {
		return tui.StatusStyle("failed").Render("⚠ usage unavailable")
	}
	// Layout per window: the usage percentage on the LEFT of the gauge, then the
	// gauge, then its legend (5h/wk) and time-to-reset on the RIGHT. Every field
	// is fixed-width — "NNN%" (4), gauge (8), the 2-char legend, and the reset
	// time padded to 5 — so the two segments stay column-aligned under the
	// right-aligned line as the numbers and durations change.
	seg := func(tag string, w usage.Window) string {
		return tui.HelpStyle.Render(fmt.Sprintf("%3.0f%% ", w.Utilization)) +
			usageGauge(w.Utilization, 8) +
			tui.HelpStyle.Render(fmt.Sprintf(" %s %5s", tag, usageRemaining(w.ResetsAt, now)))
	}
	return seg("5h", s.Usage.FiveHour) + tui.HelpStyle.Render("    ") + seg("wk", s.Usage.SevenDay)
}
