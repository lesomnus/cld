package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

// watchSpinner is the braille frame set used to animate the transient states
// (a working conversation, a provisioning container) so the view visibly ticks.
var watchSpinner = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// watchLogo is the Claude sunburst mark shown top-left in place of a title;
// watchLogoStyle paints it Claude's brand orange.
const watchLogo = "✻"

var watchLogoStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("173"))

func NewCmdWatch() *xli.Command {
	interval := time.Second
	return &xli.Command{
		Name:  "watch",
		Brief: "live view of every devcontainer's activity",
		Flags: flg.Flags{
			&flg.Duration{
				Name:    "interval",
				Alias:   'n',
				Brief:   "how often to poll the daemon (e.g. 500ms, 2s)",
				Default: &interval,
			},
			&flg.Switch{
				Name:  "no-bell",
				Brief: "do not ring the terminal bell when a container finishes (working→waiting); the bell is on by default",
			},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			iv := flg.MustGet[time.Duration](cmd, "interval")
			if iv <= 0 {
				iv = time.Second
			}
			// The bell is on by default; --no-bell (absent → false via Get) turns
			// it off. Get, not MustGet, so an absent switch reads false rather
			// than panicking on the missing default.
			noBell, _ := flg.Get[bool](cmd, "no-bell")

			m := newWatchModel(ctx, c.SocketPath(), iv)
			m.bell = !noBell

			// Without a terminal there is nothing to animate and no keys to
			// read, so print a single frame and return instead of hanging on a
			// live loop — keeps `cld watch | cat` and CI usable.
			if !term.IsTerminal(int(os.Stdout.Fd())) {
				m.now = time.Now()
				m.items, m.err = daemon.FetchItems(ctx, c.SocketPath())
				m.usage, _ = daemon.FetchUsage(ctx, c.SocketPath())
				m.loaded = true
				fmt.Fprint(os.Stdout, m.frame_view())
				return nil
			}

			p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
			_, err := p.Run()
			return err
		}),
	}
}

// watchModel drives the live listing. It polls the daemon on an interval and
// animates a spinner on a faster clock; the two are decoupled so the spinner
// keeps ticking (and the FOR durations keep counting up) between fetches.
type watchModel struct {
	ctx      context.Context
	socket   string
	interval time.Duration

	items  []daemon.Item
	err    error
	loaded bool

	// usage is the latest subscription-usage snapshot shown in the footer,
	// refreshed on its own slower cadence (watchUsageInterval) than the item
	// poll since the daemon caches usage for a minute anyway. Nil until the
	// first fetch returns; a fetch error leaves the previous snapshot in place.
	usage *daemon.UsageReport

	// bell rings the terminal bell when a container transitions working→waiting.
	// prevAct remembers each container's last-seen activity (keyed by ID) so a
	// fetch can detect that transition; it is a reference type, so it survives
	// the value-copy the tea.Model contract makes on every Update.
	bell    bool
	prevAct map[string]daemon.Activity

	now    time.Time
	frame  int
	width  int
	height int
}

func newWatchModel(ctx context.Context, socket string, interval time.Duration) watchModel {
	// Seed now so the very first frame — drawn before the first tick — shows a
	// real clock and real durations instead of the zero time.
	return watchModel{
		ctx: ctx, socket: socket, interval: interval, now: time.Now(),
		prevAct: map[string]daemon.Activity{},
	}
}

// finishedTurn reports whether any container went from working to waiting
// between the previously seen activities and items, and records the new
// activities for the next comparison. A container first seen (no prior entry)
// never counts, so startup does not ring for already-idle sessions.
func (m watchModel) finishedTurn(items []daemon.Item) bool {
	rang := false
	for _, it := range items {
		if m.prevAct[it.ID] == daemon.ActivityWorking && it.Activity == daemon.ActivityWaiting {
			rang = true
		}
	}
	// Reset to exactly the current set so a departed container's stale state
	// cannot linger and a returning one is treated as first-seen.
	clear(m.prevAct)
	for _, it := range items {
		m.prevAct[it.ID] = it.Activity
	}
	return rang
}

// ringBell writes a BEL to the terminal (stderr, so it is untouched by the
// alt-screen render on stdout). Over tmux/SSH it reaches the outer terminal,
// which turns it into an audible or visual alert per the user's config.
func ringBell() tea.Msg {
	fmt.Fprint(os.Stderr, "\a")
	return nil
}

type watchItemsMsg struct {
	items []daemon.Item
	err   error
}
type watchUsageMsg struct{ report *daemon.UsageReport }
type watchRefetchMsg struct{}
type watchTickMsg time.Time
type watchUsageTickMsg struct{}

func (m watchModel) fetch() tea.Cmd {
	return func() tea.Msg {
		items, err := daemon.FetchItems(m.ctx, m.socket)
		return watchItemsMsg{items: items, err: err}
	}
}

// fetchUsage pulls the usage report for the footer. A fetch error is swallowed
// (report stays nil / unchanged) so a usage hiccup never disturbs the listing.
func (m watchModel) fetchUsage() tea.Cmd {
	return func() tea.Msg {
		report, err := daemon.FetchUsage(m.ctx, m.socket)
		if err != nil {
			return watchUsageMsg{report: nil}
		}
		return watchUsageMsg{report: report}
	}
}

// watchUsageInterval is how often the footer's usage refreshes. It tracks the
// daemon's cache TTL exactly: the daemon serves the same memoized value until
// UsageTTL elapses, so polling any faster would only re-fetch identical data.
const watchUsageInterval = daemon.UsageTTL

func watchUsageTick() tea.Cmd {
	return tea.Tick(watchUsageInterval, func(time.Time) tea.Msg { return watchUsageTickMsg{} })
}

// watchAnim is the spinner/clock cadence: fast enough to animate smoothly and
// to advance the FOR durations by the second, independent of the poll interval.
const watchAnim = 125 * time.Millisecond

func watchTick() tea.Cmd {
	return tea.Tick(watchAnim, func(t time.Time) tea.Msg { return watchTickMsg(t) })
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.fetch(), m.fetchUsage(), watchTick(), watchUsageTick())
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			// Force an out-of-band refresh without waiting for the interval.
			return m, tea.Batch(m.fetch(), m.fetchUsage())
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case watchTickMsg:
		m.now = time.Time(msg)
		m.frame++
		return m, watchTick()
	case watchItemsMsg:
		// Detect working→waiting transitions before replacing the item set, and
		// ring the bell (if enabled) alongside scheduling the next poll.
		finished := m.finishedTurn(msg.items)
		m.items, m.err, m.loaded = msg.items, msg.err, true
		// Schedule the next poll relative to this reply so a slow daemon can
		// never stack overlapping fetches.
		next := tea.Tick(m.interval, func(time.Time) tea.Msg { return watchRefetchMsg{} })
		if finished && m.bell {
			return m, tea.Batch(next, ringBell)
		}
		return m, next
	case watchRefetchMsg:
		return m, m.fetch()
	case watchUsageMsg:
		// Keep the prior snapshot on a nil (errored) report so the footer does
		// not blink to empty on a transient failure.
		if msg.report != nil {
			m.usage = msg.report
		}
		return m, nil
	case watchUsageTickMsg:
		return m, tea.Batch(m.fetchUsage(), watchUsageTick())
	}
	return m, nil
}

func (m watchModel) View() string { return m.frame_view() }

// frame_view renders one complete frame: a summary header, the aligned table,
// and a key-hint footer. It is also used for the single non-interactive dump.
func (m watchModel) frame_view() string {
	var b strings.Builder
	b.WriteString(m.header())
	b.WriteByte('\n')

	if !m.loaded {
		b.WriteString(tui.HelpStyle.Render("  loading…"))
		b.WriteByte('\n')
		return b.String()
	}
	if m.err != nil {
		b.WriteString(tui.StatusStyle("failed").Render("  cannot reach daemon: " + m.err.Error()))
		b.WriteByte('\n')
		if len(m.items) == 0 {
			return b.String()
		}
	}
	if len(m.items) == 0 {
		b.WriteString(tui.HelpStyle.Render("  no devcontainers"))
		b.WriteByte('\n')
		return b.String()
	}

	b.WriteByte('\n')
	b.WriteString(m.table())

	return m.pinBottom(b.String(), m.bottomBlock())
}

// bottomBlock builds the lines pinned to the screen bottom: one right-aligned
// subscription-usage bar per login, or nil when there is none. Each bar is sized
// to the full width and degrades to fit it — narrowing first drops the
// percentage, then shrinks the gauge (see usageBar). The in/out/cw totals used
// to share this row but now live in the header, left of the clock (see header).
func (m watchModel) bottomBlock() []string {
	if m.usage == nil || len(m.usage.Sources) == 0 {
		return nil
	}
	lines := make([]string, len(m.usage.Sources))
	for i, s := range m.usage.Sources {
		lines[i] = rightAlign(usageBar(s, m.now, m.width), m.width)
	}
	return lines
}

// rightAlign pads s with leading spaces so it ends at column width. A zero or
// too-small width leaves s untouched (piped output, or a bar already too wide).
func rightAlign(s string, width int) string {
	if pad := width - lipgloss.Width(s); width > 0 && pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

// watchUsageTotals is the fleet-wide consumption summary shown at the bottom
// left: the daemon's persisted weekly tally of input, output, and cache-write
// tokens (see daemon.weeklyStore), labeled with the same glyphs as the table's
// in/out/cw columns (and, like them, leaving cacheRead out). Unlike the table's
// per-container numbers this is "this week" and survives a daemon restart. Empty
// when the report is missing or the window is still untouched, so the line
// collapses rather than printing three zeros.
func watchUsageTotals(report *daemon.UsageReport, compact bool) string {
	if report == nil || report.Weekly.Empty() {
		return ""
	}
	w := report.Weekly
	// Each value sits in a fixed-width, right-aligned slot with its glyph on the
	// RIGHT, so the numbers line up in a column and the glyphs read as a trailing
	// unit — no separators needed between the three.
	part := func(v int64, glyph string) string {
		return fmt.Sprintf("%*s%s", watchTokenColWidth, formatTokens(v), glyph)
	}
	// compact keeps only the cache-write figure — the one the narrow table also
	// keeps — so the line still says something when there is no room for three.
	if compact {
		return tui.HelpStyle.Render(part(w.CacheCreation, watchCacheWriteHeader))
	}
	parts := []string{
		part(w.Input, watchInHeader),
		part(w.Output, watchOutHeader),
		part(w.CacheCreation, watchCacheWriteHeader),
	}
	return tui.HelpStyle.Render(strings.Join(parts, "  "))
}

// pinBottom places the bottom lines on the last rows of the alt-screen, filling
// the gap under the top content with blank lines so the usage bars sit at the
// very bottom. With no known height (piped output) it just appends them under
// the content.
func (m watchModel) pinBottom(top string, bottom []string) string {
	trimmed := strings.TrimRight(top, "\n")
	if len(bottom) == 0 {
		return trimmed + "\n"
	}
	if m.height <= 0 {
		return trimmed + "\n\n" + strings.Join(bottom, "\n") + "\n"
	}
	lines := strings.Split(trimmed, "\n")
	gap := max(m.height-len(lines)-len(bottom), 1)
	for range gap {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	return strings.Join(lines, "\n")
}

// header is the top line: the Claude mark on the left, and on the right the
// fleet-wide weekly in/out/cw totals followed by the clock. The logo stays far
// left; the totals+clock group is right-aligned, so the totals sit just left of
// the clock.
func (m watchModel) header() string {
	logo := watchLogoStyle.Render(watchLogo)
	clock := tui.HelpStyle.Render(m.now.Format("15:04:05"))
	right := clock
	if totals := m.headerTotals(); totals != "" {
		right = totals + "  " + clock
	}
	if gap := m.width - lipgloss.Width(logo) - lipgloss.Width(right); m.width > 0 && gap > 1 {
		return logo + strings.Repeat(" ", gap) + right
	}
	return logo + "   " + right
}

// headerTotals is the weekly in/out/cw tally shown left of the clock: the full
// three when they fit beside the logo and clock, the compact cache-write-only
// form when they do not, and nothing when even that would not fit.
func (m watchModel) headerTotals() string {
	full := watchUsageTotals(m.usage, false)
	if full == "" || m.width <= 0 {
		return full
	}
	// Space the right group needs: logo, a one-space minimum gap after it, the
	// totals, two spaces, and the 8-char clock.
	fits := func(s string) bool {
		return lipgloss.Width(watchLogo)+1+lipgloss.Width(s)+2+8 <= m.width
	}
	if fits(full) {
		return full
	}
	if compact := watchUsageTotals(m.usage, true); fits(compact) {
		return compact
	}
	return ""
}

// watchName merges a container's alias into the tail of its name, so one column
// carries both. The alias is derived FROM the name — segment initials ("lc" for
// "lesomnus-cld"), a truncation, or the whole name — so its last character is
// almost always the first character of the name's last segment. Overlapping
// them there prints the alias, in the accent color, running straight into the
// rest of that segment: "lc"+"cld" reads as "lcld", "avln"+"name" as "avlname",
// "mcp"+"project" as "mcproject".
//
// The overlap is the longest suffix of the alias that starts the last segment,
// matched case-insensitively so a name carrying capitals still merges (the
// alias is always lowercased, the name is not). Every alias form Alias produces
// overlaps by at least one character; the exception is a collision-broken alias
// ("lc-3f"), which shares nothing with the name. That falls back to the two
// fields side by side — exactly how they read as separate columns today — so
// nothing is ever silently fused into a word that is neither.
func watchName(it daemon.Item) string {
	alias, seg := it.Alias, devc.LastSegment(it.Name)
	if alias == "" || seg == "" {
		if alias != "" {
			return cardAliasStyle.Render(alias)
		}
		return it.Name
	}

	lower := strings.ToLower(alias)
	for n := min(len(alias), len(seg)); n > 0; n-- {
		if strings.HasSuffix(lower, strings.ToLower(seg[:n])) {
			return cardAliasStyle.Render(alias) + seg[n:]
		}
	}
	return cardAliasStyle.Render(alias) + " " + seg
}

// watchAliasCell is the narrow-screen NAME cell: just the alias (in the accent
// color), which is what the merged watchName leads with anyway. A container with
// no alias yet falls back to its name's last segment so the row is never blank.
func watchAliasCell(it daemon.Item) string {
	if it.Alias != "" {
		return cardAliasStyle.Render(it.Alias)
	}
	if seg := devc.LastSegment(it.Name); seg != "" {
		return seg
	}
	return it.Name
}

// table renders the aligned rows. Columns are ACTIVITY, NAME, FOR (⟳/⟳+),
// [IN, OUT, CW, CW-RATE,] [WORKFLOWS]; every column is padded to its widest cell.
// The consumption and WORKFLOWS columns collapse entirely when no container has
// anything to show there.
//
// When the frame is too narrow to hold the full row, columns are shed in a fixed
// order until it fits: first the WORKFLOWS glyph, then the in/out (↓/↑) pair,
// then NAME drops from the merged name to the alias alone. The always-kept core
// is activity, name, working-time, and the cache-write columns — the cache write
// being the one consumption signal worth watching (see the token-type comment).
func (m watchModel) table() string {
	n := len(m.items)
	type column struct {
		header   string
		cells    []string
		right    bool // right-align header and cells (for the numeric columns)
		minWidth int  // floor on the column width, so it does not jitter frame to frame
		width    int  // resolved width: max of header, minWidth, and every cell
	}
	measure := func(c column) column {
		c.width = max(lipgloss.Width(c.header), c.minWidth)
		for _, cell := range c.cells {
			c.width = max(c.width, lipgloss.Width(cell))
		}
		return c
	}

	// The activity column has no header and shows only the status glyph (no word).
	act := column{header: "", cells: make([]string, n)}
	wf := column{header: watchWorkflowHeader, cells: make([]string, n)}
	// The two working-time columns are right-aligned and held at a fixed width
	// so the columns after them do not shift as the durations tick.
	workLast := column{header: watchWorkLastHeader, cells: make([]string, n), right: true, minWidth: watchDurationWidth}
	workTotal := column{header: watchWorkTotalHeader, cells: make([]string, n), right: true, minWidth: watchDurationWidth}
	// NAME has two forms: the merged alias+name (watchName) and, when squeezed,
	// the alias alone (watchAliasCell).
	nameFull := column{header: "NAME", cells: make([]string, n)}
	nameAlias := column{header: "NAME", cells: make([]string, n)}
	// Consumption, right-aligned so the magnitudes line up down the list, and
	// held at a fixed width so the columns do not jitter as the numbers change.
	// Fresh input, generated output, cache writes, and the live cache-write rate
	// — cacheRead is deliberately omitted (see the token-type constants).
	in := column{header: watchInHeader, cells: make([]string, n), right: true, minWidth: watchTokenColWidth}
	out := column{header: watchOutHeader, cells: make([]string, n), right: true, minWidth: watchTokenColWidth}
	cw := column{header: watchCacheWriteHeader, cells: make([]string, n), right: true, minWidth: watchTokenColWidth}
	cwRate := column{header: watchCacheWriteRateHeader, cells: make([]string, n), right: true, minWidth: watchTokenColWidth + 1}

	anyWf := false
	anyTel := false
	for i, it := range m.items {
		glyph, style := m.activityCell(it)
		act.cells[i] = style.Render(glyph)
		if c := watchWorkflowCell(it, m.now); c != "" {
			wf.cells[i] = c
			anyWf = true
		}
		workLast.cells[i] = tui.HelpStyle.Render(watchWorkLast(it, m.now))
		workTotal.cells[i] = tui.HelpStyle.Render(watchWorkTotal(it, m.now))
		nameFull.cells[i] = watchName(it)
		nameAlias.cells[i] = watchAliasCell(it)
		// A container that never reported leaves the cells empty rather than
		// showing a zero, which would claim it was measured and found idle.
		if t := it.Telemetry; t != nil {
			anyTel = true
			in.cells[i] = tui.HelpStyle.Render(formatTokens(t.ByType[daemon.TokenTypeInput]))
			out.cells[i] = tui.HelpStyle.Render(formatTokens(t.ByType[daemon.TokenTypeOutput]))
			cw.cells[i] = tui.HelpStyle.Render(formatTokens(t.ByType[daemon.TokenTypeCacheCreation]))
			// The rate cell brightens with the rate (see tui.RateStyle), so a
			// container actively churning the cache stands out down the column.
			cwRate.cells[i] = tui.RateStyle(t.CacheCreationPerMin).Render(formatRate(t.CacheCreationPerMin))
		}
	}

	act, workLast, workTotal = measure(act), measure(workLast), measure(workTotal)
	nameFull, nameAlias = measure(nameFull), measure(nameAlias)
	in, out, cw, cwRate, wf = measure(in), measure(out), measure(cw), measure(cwRate), measure(wf)

	const gap = 2
	// rowWidth is the rendered width of a column set: the two-space indent, each
	// column, and the gap after every column but the last (one space after the
	// lone activity glyph, two elsewhere).
	rowWidth := func(cols []column) int {
		w := 2
		for i := range cols {
			w += cols[i].width
			switch {
			case i == len(cols)-1:
			case i == 0:
				w++
			default:
				w += gap
			}
		}
		return w
	}
	// build assembles the visible columns for a degradation tier. alias swaps NAME
	// to the alias-only form; showInOut and showWf keep those columns.
	build := func(showWf, showInOut, alias bool) []column {
		name := nameFull
		if alias {
			name = nameAlias
		}
		cols := []column{act, name, workLast, workTotal}
		if anyTel {
			if showInOut {
				cols = append(cols, in, out)
			}
			cols = append(cols, cw, cwRate)
		}
		if anyWf && showWf {
			cols = append(cols, wf)
		}
		return cols
	}
	// Richest first; shed workflow, then in/out, then the full name — stopping at
	// the first tier that fits (or the narrowest if none do).
	tiers := []struct{ wf, inOut, alias bool }{
		{true, true, false},
		{false, true, false},
		{false, false, false},
		{false, false, true},
	}
	cols := build(true, true, false)
	for _, t := range tiers {
		cols = build(t.wf, t.inOut, t.alias)
		if m.width <= 0 || rowWidth(cols) <= m.width {
			break
		}
	}

	sepAfter := func(c int) string {
		switch {
		case c == len(cols)-1:
			return ""
		case c == 0:
			return " "
		default:
			return strings.Repeat(" ", gap)
		}
	}
	pad := func(s string, w int, right bool) string {
		d := w - lipgloss.Width(s)
		if d <= 0 {
			return s
		}
		if right {
			return strings.Repeat(" ", d) + s
		}
		return s + strings.Repeat(" ", d)
	}

	var b strings.Builder
	var head strings.Builder
	for c := range cols {
		head.WriteString(pad(cols[c].header, cols[c].width, cols[c].right))
		head.WriteString(sepAfter(c))
	}
	b.WriteString("  ")
	b.WriteString(tui.HelpStyle.Render(strings.TrimRight(head.String(), " ")))
	b.WriteByte('\n')

	for i := range m.items {
		b.WriteString("  ")
		var row strings.Builder
		for c := range cols {
			row.WriteString(pad(cols[c].cells[i], cols[c].width, cols[c].right))
			row.WriteString(sepAfter(c))
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// watchWorkflowStale is how long a not-yet-finalized run's newest write may sit
// idle before it is treated as crashed rather than live. It only gates runs
// with no state file (a finalized run is classified authoritatively), and is
// generous because a single long agent can leave a run quiet mid-flight.
const watchWorkflowStale = 5 * time.Minute

type workflowBucket int

const (
	workflowDone    workflowBucket = iota // finished cleanly (or finalized, no failure)
	workflowLive                          // still running
	workflowProblem                       // aborted, failed, or crashed
)

// workflowFailureStatus is the set of state-file status words that mark a
// finalized run as unsuccessful. Anything else (including an unread/empty
// status) is taken as success, so a best-effort status misread never turns a
// good run red.
var workflowFailureStatus = map[string]bool{
	"failed": true, "error": true, "errored": true,
	"cancelled": true, "canceled": true, "aborted": true,
}

// classifyWorkflowRun decides how a single run should be shown. It trusts the
// state file first: a finalized run is never "live", even if its files were
// touched a moment ago. Only a run with no state file whose newest write is
// recent is live — which correctly keeps a sequential workflow that is momentarily
// balanced (every started agent has returned, next not launched yet) out of the
// "done" bucket it would otherwise fall into.
func classifyWorkflowRun(w daemon.WorkflowRun, now time.Time) workflowBucket {
	if w.Finalized {
		if w.Running() > 0 || workflowFailureStatus[w.Status] {
			return workflowProblem // orphaned agents, or an explicit failure status
		}
		return workflowDone
	}
	if !w.UpdatedAt.IsZero() && now.Sub(w.UpdatedAt) < watchWorkflowStale {
		return workflowLive
	}
	return workflowProblem // no state file and gone quiet: crashed mid-run
}

// watchWorkflowHeader labels the WORKFLOWS column with a compact glyph — many
// dots for the many agents a run fans out — so the header does not dominate the
// row width the way the full word did.
const watchWorkflowHeader = "⁙"

// The consumption column headers are glyphs: ↓ input (tokens coming IN), ↑
// output (tokens going OUT), ● cache write (the cacheCreation type; cacheRead
// has no column). lipgloss measures each as one cell, which is what the column
// padding is computed from. Note these are East-Asian *Ambiguous* width: a
// terminal configured to draw Ambiguous glyphs two cells wide (some CJK locales)
// will shift the columns after them by one — an accepted trade for the clearer
// glyphs. The rate's denominator lives in its header rather than in every cell
// (see formatRate), which is both narrower and where a unit belongs in a table.
const (
	watchInHeader             = "↓"
	watchOutHeader            = "↑"
	watchCacheWriteHeader     = "●"
	watchCacheWriteRateHeader = "●" + rateUnit
)

// watchTokenColWidth fixes the in/out/cw columns to a stable width so they do
// not jitter frame to frame as the numbers grow and shrink. formatTokens caps
// at three significant figures ("999.9k", "1.2M"), so six cells always holds a
// value with room for the glyph header.
const watchTokenColWidth = 6

// watchWorkflowCell summarizes a container's workflow runs as "<finished>/<total>":
// how many of the runs launched are no longer live over the total number of runs.
// Finished lumps every non-live outcome together — a clean completion, a failure,
// and a crash all count the same — because the row only needs the parallel batch's
// progress, not its success breakdown. Styled active while any run is still live,
// dim once all have finished. Empty when the container has run no workflows, which
// collapses the column.
func watchWorkflowCell(it daemon.Item, now time.Time) string {
	total := len(it.Workflows)
	if total == 0 {
		return ""
	}
	live := 0
	for _, w := range it.Workflows {
		if classifyWorkflowRun(w, now) == workflowLive {
			live++
		}
	}
	style := tui.HelpStyle
	if live > 0 {
		style = cardWorkingStyle
	}
	return style.Render(fmt.Sprintf("%d/%d", total-live, total))
}

// activityCell returns the glyph and style for a container's leading cell. A
// ready container shows its live conversation activity (working spins); any
// other container shows its lifecycle state (provisioning spins). Only the
// symbol is shown — the status word is dropped from the table.
func (m watchModel) activityCell(it daemon.Item) (string, lipgloss.Style) {
	if it.Status == daemon.StatusReady {
		glyph, style := activityLook(it.Activity)
		if it.Activity == daemon.ActivityWorking {
			glyph = watchSpinner[m.frame%len(watchSpinner)]
		}
		return glyph, style
	}

	style := tui.StatusStyle(string(it.Status))
	switch it.Status {
	case daemon.StatusProvisioning:
		return watchSpinner[m.frame%len(watchSpinner)], style
	case daemon.StatusFailed:
		return "✗", style
	case daemon.StatusStopped:
		return "▪", style
	case daemon.StatusSessionEnded:
		return "◌", style
	default:
		return "·", style
	}
}

// watchDurationWidth is the fixed width of each working-time column, sized for
// the widest duration they print ("59m59s"/"23h59m"), so shorter values
// right-align within it and the columns after them never shift as the durations
// tick.
const watchDurationWidth = 6

// watchWorkLastHeader and watchWorkTotalHeader label the two working-time
// columns. Both reuse "⟳" — the same glyph activityLook already draws for a
// working conversation, so it is already proven to render at one cell in the
// terminals cld runs in (unlike the stopwatch and hourglass pictographs, which
// default to emoji presentation and would be drawn double-width and colored).
// The bare glyph is the CURRENT or most recent stint; "+" marks the one that
// accumulates, matching how "#/m" qualifies "#".
const (
	watchWorkLastHeader  = "⟳"
	watchWorkTotalHeader = "⟳+"
)

// watchWorkLast is how long the conversation's current generating stint has been
// running, or — when it is not generating — how long the most recent one ran.
// While working the value ticks live from ActivitySince, so a long turn visibly
// grows without the daemon having to republish.
//
// An empty cell means there is nothing to report yet: no stint has completed and
// none is running. That includes a poll-only (cross-arch) container, whose
// activity transitions the daemon never observes — showing a "0s" there would
// claim it was measured and found instant.
func watchWorkLast(it daemon.Item, now time.Time) string {
	if d, ok := watchLiveStint(it, now); ok {
		return watchFormatDuration(d)
	}
	if it.WorkLast <= 0 {
		return ""
	}
	return watchFormatDuration(it.WorkLast)
}

// watchWorkTotal is every completed generating stint summed, plus the one in
// progress so the total advances during a turn rather than jumping when it ends.
func watchWorkTotal(it daemon.Item, now time.Time) string {
	d := it.WorkTotal
	if live, ok := watchLiveStint(it, now); ok {
		d += live
	}
	if d <= 0 {
		return ""
	}
	return watchFormatDuration(d)
}

// watchLiveStint is the age of a generating stint still in progress. ok is false
// unless the container is ready, actually working, and carries a real transition
// mark — the last of which excludes a poll-only container, whose Activity is
// classified at listing time and has no moment behind it.
func watchLiveStint(it daemon.Item, now time.Time) (time.Duration, bool) {
	if it.Status != daemon.StatusReady || it.Activity != daemon.ActivityWorking || it.ActivitySince.IsZero() {
		return 0, false
	}
	return max(now.Sub(it.ActivitySince), 0), true
}

// watchFormatDuration renders a duration compactly, widening its unit as it
// grows so the cell stays inside watchDurationWidth.
func watchFormatDuration(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		if s := int(d.Seconds()) % 60; s != 0 {
			return fmt.Sprintf("%dm%02ds", int(d.Minutes()), s)
		}
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		if mm := int(d.Minutes()) % 60; mm != 0 {
			return fmt.Sprintf("%dh%02dm", int(d.Hours()), mm)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		// A cumulative total genuinely reaches days, where a bare "1d" would
		// hide anything up to 23 hours of generating. Carry the hours like the
		// smaller units carry theirs; "2d02h" still fits watchDurationWidth.
		if h := int(d.Hours()) % 24; h != 0 {
			return fmt.Sprintf("%dd%02dh", int(d.Hours())/24, h)
		}
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}
