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
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			iv := flg.MustGet[time.Duration](cmd, "interval")
			if iv <= 0 {
				iv = time.Second
			}

			m := newWatchModel(ctx, c.SocketPath(), iv)

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

	now    time.Time
	frame  int
	width  int
	height int
}

func newWatchModel(ctx context.Context, socket string, interval time.Duration) watchModel {
	return watchModel{ctx: ctx, socket: socket, interval: interval}
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
		m.items, m.err, m.loaded = msg.items, msg.err, true
		// Schedule the next poll relative to this reply so a slow daemon can
		// never stack overlapping fetches.
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return watchRefetchMsg{} })
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

// table renders the aligned rows. Columns are ACTIVITY, FOR, ALIAS, NAME,
// TITLE; every column but TITLE is padded to its widest cell, and TITLE is
// truncated to whatever width remains.
func (m watchModel) table() string {
	type cell struct {
		act, forr, alias, name, title string
	}
	cells := make([]cell, len(m.items))
	wAct, wFor, wAlias, wName := lipgloss.Width("ACTIVITY"), lipgloss.Width("FOR"), lipgloss.Width("ALIAS"), lipgloss.Width("NAME")
	for i, it := range m.items {
		glyph, word, style := m.activityCell(it)
		act := style.Render(glyph + " " + word)
		forr := watchDuration(it, m.now)
		cells[i] = cell{act: act, forr: forr, alias: it.Alias, name: it.Name, title: it.Title}
		wAct = max(wAct, lipgloss.Width(act))
		wFor = max(wFor, lipgloss.Width(forr))
		wAlias = max(wAlias, lipgloss.Width(it.Alias))
		wName = max(wName, lipgloss.Width(it.Name))
	}

	// TITLE gets the leftover width. gap is the two-space separator between
	// each of the five columns.
	const gap = 2
	titleBudget := 0
	if m.width > 0 {
		used := 2 + wAct + gap + wFor + gap + wAlias + gap + wName + gap // 2 = leading indent
		titleBudget = m.width - used
	}

	pad := func(s string, w int) string {
		if d := w - lipgloss.Width(s); d > 0 {
			return s + strings.Repeat(" ", d)
		}
		return s
	}
	sep := strings.Repeat(" ", gap)

	var b strings.Builder
	head := "  " + tui.HelpStyle.Render(
		pad("ACTIVITY", wAct)+sep+pad("FOR", wFor)+sep+pad("ALIAS", wAlias)+sep+pad("NAME", wName)+sep+"TITLE")
	b.WriteString(head)
	b.WriteByte('\n')

	for _, c := range cells {
		title := c.title
		if title == "" {
			title = tui.HelpStyle.Render("—")
		} else if titleBudget > 0 {
			title = tui.HelpStyle.Render(watchTruncate(title, titleBudget))
		} else {
			title = tui.HelpStyle.Render(title)
		}
		b.WriteString("  ")
		b.WriteString(pad(c.act, wAct))
		b.WriteString(sep)
		b.WriteString(pad(tui.HelpStyle.Render(c.forr), wFor))
		b.WriteString(sep)
		b.WriteString(pad(cardAliasStyle.Render(c.alias), wAlias))
		b.WriteString(sep)
		b.WriteString(pad(c.name, wName))
		b.WriteString(sep)
		b.WriteString(title)
		b.WriteByte('\n')
	}
	return b.String()
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
