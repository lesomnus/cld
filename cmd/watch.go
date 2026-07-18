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
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

// watchSpinner is the braille frame set used to animate the transient states
// (a working conversation, a provisioning container) so the view visibly ticks.
var watchSpinner = []string{"⣾", "⣽", "⣻", "⢿", "⡿", "⣟", "⣯", "⣷"}

// watchStatusOrder fixes the summary/glyph ordering of the non-ready lifecycle
// states so the header counts read the same every frame.
var watchStatusOrder = []daemon.Status{
	daemon.StatusProvisioning,
	daemon.StatusStopped,
	daemon.StatusSessionEnded,
	daemon.StatusFailed,
}

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
type watchRefetchMsg struct{}
type watchTickMsg time.Time

func (m watchModel) fetch() tea.Cmd {
	return func() tea.Msg {
		items, err := daemon.FetchItems(m.ctx, m.socket)
		return watchItemsMsg{items: items, err: err}
	}
}

// watchAnim is the spinner/clock cadence: fast enough to animate smoothly and
// to advance the FOR durations by the second, independent of the poll interval.
const watchAnim = 125 * time.Millisecond

func watchTick() tea.Cmd {
	return tea.Tick(watchAnim, func(t time.Time) tea.Msg { return watchTickMsg(t) })
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.fetch(), watchTick())
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "r":
			// Force an out-of-band refresh without waiting for the interval.
			return m, m.fetch()
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
	b.WriteByte('\n')
	b.WriteString(tui.HelpStyle.Render(fmt.Sprintf("  [q] quit  [r] refresh  ·  every %s", m.interval)))
	b.WriteByte('\n')
	return b.String()
}

// header is the top line: the title, the container count, per-state counts, and
// a right-aligned clock.
func (m watchModel) header() string {
	var working, waiting, idle int
	byStatus := map[daemon.Status]int{}
	for _, it := range m.items {
		if it.Status != daemon.StatusReady {
			byStatus[it.Status]++
			continue
		}
		switch it.Activity {
		case daemon.ActivityWorking:
			working++
		case daemon.ActivityIdle:
			idle++
		default:
			waiting++
		}
	}

	segs := []string{}
	if working > 0 {
		segs = append(segs, cardWorkingStyle.Render(fmt.Sprintf("%d working", working)))
	}
	if waiting > 0 {
		segs = append(segs, tui.HelpStyle.Render(fmt.Sprintf("%d waiting", waiting)))
	}
	if idle > 0 {
		segs = append(segs, tui.HelpStyle.Render(fmt.Sprintf("%d idle", idle)))
	}
	for _, s := range watchStatusOrder {
		if n := byStatus[s]; n > 0 {
			segs = append(segs, tui.StatusStyle(string(s)).Render(fmt.Sprintf("%d %s", n, s)))
		}
	}

	dot := tui.HelpStyle.Render("  ·  ")
	noun := "devcontainers"
	if len(m.items) == 1 {
		noun = "devcontainer"
	}
	left := tui.TitleStyle.Render("cld watch") +
		tui.HelpStyle.Render(fmt.Sprintf("  ·  %d %s", len(m.items), noun))
	if len(segs) > 0 {
		left += dot + strings.Join(segs, tui.HelpStyle.Render(" · "))
	}

	clock := tui.HelpStyle.Render(m.now.Format("15:04:05"))
	if gap := m.width - lipgloss.Width(left) - lipgloss.Width(clock); m.width > 0 && gap > 1 {
		return left + strings.Repeat(" ", gap) + clock
	}
	return left + "   " + clock
}

// table renders the aligned rows. Columns are ACTIVITY, [WORKFLOWS,] FOR,
// ALIAS, NAME, TITLE; every column but TITLE is padded to its widest cell, and
// TITLE is truncated to whatever width remains. The WORKFLOWS column collapses
// entirely when no container is running any workflow, so the common case stays
// narrow.
func (m watchModel) table() string {
	n := len(m.items)
	type column struct {
		header string
		cells  []string
	}
	act := column{header: "ACTIVITY", cells: make([]string, n)}
	wf := column{header: "WORKFLOWS", cells: make([]string, n)}
	forc := column{header: "FOR", cells: make([]string, n)}
	alias := column{header: "ALIAS", cells: make([]string, n)}
	name := column{header: "NAME", cells: make([]string, n)}
	titles := make([]string, n)

	anyWf := false
	for i, it := range m.items {
		glyph, word, style := m.activityCell(it)
		act.cells[i] = style.Render(glyph + " " + word)
		if c := watchWorkflowCell(it, m.now); c != "" {
			wf.cells[i] = c
			anyWf = true
		}
		forc.cells[i] = tui.HelpStyle.Render(watchDuration(it, m.now))
		alias.cells[i] = cardAliasStyle.Render(it.Alias)
		name.cells[i] = it.Name
		titles[i] = it.Title
	}

	cols := []column{act}
	if anyWf {
		cols = append(cols, wf)
	}
	cols = append(cols, forc, alias, name)

	widths := make([]int, len(cols))
	for c := range cols {
		widths[c] = lipgloss.Width(cols[c].header)
		for _, cell := range cols[c].cells {
			widths[c] = max(widths[c], lipgloss.Width(cell))
		}
	}

	const gap = 2
	sep := strings.Repeat(" ", gap)

	// TITLE gets the leftover width after every fixed column plus the leading
	// two-space indent.
	titleBudget := 0
	if m.width > 0 {
		used := 2
		for c := range cols {
			used += widths[c] + gap
		}
		titleBudget = m.width - used
	}

	pad := func(s string, w int) string {
		if d := w - lipgloss.Width(s); d > 0 {
			return s + strings.Repeat(" ", d)
		}
		return s
	}

	var b strings.Builder
	heads := make([]string, len(cols))
	for c := range cols {
		heads[c] = pad(cols[c].header, widths[c])
	}
	b.WriteString("  ")
	b.WriteString(tui.HelpStyle.Render(strings.Join(heads, sep) + sep + "TITLE"))
	b.WriteByte('\n')

	for i := range m.items {
		b.WriteString("  ")
		for c := range cols {
			b.WriteString(pad(cols[c].cells[i], widths[c]))
			b.WriteString(sep)
		}
		switch {
		case titles[i] == "":
			b.WriteString(tui.HelpStyle.Render("—"))
		case m.width <= 0:
			// Width unknown (piped output): render the full title and let the
			// consumer wrap it.
			b.WriteString(tui.HelpStyle.Render(titles[i]))
		case titleBudget > 0:
			b.WriteString(tui.HelpStyle.Render(watchTruncate(titles[i], titleBudget)))
		default:
			// Width known but the fixed columns already fill it: omit the title
			// rather than render it full and overflow the row further.
		}
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

// watchWorkflowCell summarizes a container's workflow runs: live runs with
// their agent progress, then any failed/stalled runs, then a tally of completed
// ones. Empty when the container has run no workflows, which collapses the
// column.
func watchWorkflowCell(it daemon.Item, now time.Time) string {
	if len(it.Workflows) == 0 {
		return ""
	}
	var live, problem, done, agDone, agTotal int
	for _, w := range it.Workflows {
		switch classifyWorkflowRun(w, now) {
		case workflowLive:
			live++
			agDone += w.Done
			agTotal += w.Total
		case workflowProblem:
			problem++
		default:
			done++
		}
	}
	parts := make([]string, 0, 3)
	if live > 0 {
		parts = append(parts, cardWorkingStyle.Render(fmt.Sprintf("▶%d %d/%d", live, agDone, agTotal)))
	}
	if problem > 0 {
		parts = append(parts, tui.StatusStyle("failed").Render(fmt.Sprintf("⚠%d", problem)))
	}
	if done > 0 {
		parts = append(parts, tui.HelpStyle.Render(fmt.Sprintf("✓%d", done)))
	}
	return strings.Join(parts, " ")
}

// activityCell returns the glyph, word, and style for a container's leading
// cell. A ready container shows its live conversation activity (working spins);
// any other container shows its lifecycle state (provisioning spins).
func (m watchModel) activityCell(it daemon.Item) (string, string, lipgloss.Style) {
	if it.Status == daemon.StatusReady {
		glyph, style := activityLook(it.Activity)
		if it.Activity == daemon.ActivityWorking {
			glyph = watchSpinner[m.frame%len(watchSpinner)]
		}
		return glyph, string(it.Activity), style
	}

	style := tui.StatusStyle(string(it.Status))
	switch it.Status {
	case daemon.StatusProvisioning:
		return watchSpinner[m.frame%len(watchSpinner)], string(it.Status), style
	case daemon.StatusFailed:
		return "✗", string(it.Status), style
	case daemon.StatusStopped:
		return "▪", string(it.Status), style
	case daemon.StatusSessionEnded:
		return "◌", string(it.Status), style
	default:
		return "·", string(it.Status), style
	}
}

// watchDuration is the FOR cell: how long the container has held its current
// state. For a ready container that is the conversation activity's age; for any
// other state it is the lifecycle state's age. A zero mark — an activity the
// daemon never observed changing, i.e. a poll-only cross-arch container — shows
// "—" rather than a fabricated duration.
func watchDuration(it daemon.Item, now time.Time) string {
	since := it.StatusSince
	if it.Status == daemon.StatusReady {
		since = it.ActivitySince
	}
	if since.IsZero() {
		return "—"
	}
	d := max(now.Sub(since), 0)
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
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

// watchTruncate clips s to at most w display columns, appending an ellipsis when
// it cuts. It assumes title text of width-1 runes, which is the common case.
func watchTruncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}
