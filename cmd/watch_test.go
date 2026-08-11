package cmd

import (
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/require"
)

func TestWatchFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m30s"},
		{3 * time.Minute, "3m"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
		{3 * time.Hour, "3h"},
		{48 * time.Hour, "2d"},
		{50 * time.Hour, "2d02h"},
		{-time.Minute, "0s"}, // clamped rather than rendered negative
	} {
		require.Equalf(t, tc.want, watchFormatDuration(tc.d), "watchFormatDuration(%s)", tc.d)
		require.LessOrEqualf(t, len(watchFormatDuration(tc.d)), watchDurationWidth,
			"%s must fit the fixed column width", tc.d)
	}
}

func TestWatchWorkingTimes(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	ready := func(a daemon.Activity) daemon.Item {
		return daemon.Item{Status: daemon.StatusReady, Activity: a}
	}

	t.Run("while working both tick from the live stint", func(t *testing.T) {
		it := ready(daemon.ActivityWorking)
		it.ActivitySince = ago(90 * time.Second)
		it.WorkTotal = 5 * time.Minute // banked from earlier turns
		it.WorkLast = 30 * time.Second

		// The in-flight stint replaces the last completed one, and adds to the
		// total — so neither figure sits frozen through a long turn.
		require.Equal(t, "1m30s", watchWorkLast(it, now))
		require.Equal(t, "6m30s", watchWorkTotal(it, now))
	})

	t.Run("once the turn ends both read the banked figures", func(t *testing.T) {
		it := ready(daemon.ActivityWaiting)
		it.ActivitySince = ago(time.Hour) // waiting for an hour: must not count
		it.WorkTotal = 6*time.Minute + 30*time.Second
		it.WorkLast = 90 * time.Second

		require.Equal(t, "1m30s", watchWorkLast(it, now))
		require.Equal(t, "6m30s", watchWorkTotal(it, now))
	})

	t.Run("a session that never generated shows nothing, not a zero", func(t *testing.T) {
		it := ready(daemon.ActivityIdle)
		it.ActivitySince = ago(time.Hour)
		require.Equal(t, "", watchWorkLast(it, now))
		require.Equal(t, "", watchWorkTotal(it, now))
	})

	t.Run("a poll-only container is blank rather than fabricated", func(t *testing.T) {
		// Cross-arch: classified as working at listing time, but no transition
		// was ever observed, so there is no moment to measure from.
		it := ready(daemon.ActivityWorking) // ActivitySince deliberately zero
		require.Equal(t, "", watchWorkLast(it, now))
		require.Equal(t, "", watchWorkTotal(it, now))
	})

	t.Run("a stopped container keeps its totals but has no live stint", func(t *testing.T) {
		it := daemon.Item{Status: daemon.StatusStopped, Activity: daemon.ActivityWorking,
			ActivitySince: ago(time.Hour), WorkTotal: 2 * time.Minute, WorkLast: time.Minute}
		require.Equal(t, "1m", watchWorkLast(it, now))
		require.Equal(t, "2m", watchWorkTotal(it, now), "a stale working flag must not keep accruing")
	})
}

func TestWatchWorkflowCell(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	t.Run("no workflows collapses to empty", func(t *testing.T) {
		require.Equal(t, "", watchWorkflowCell(daemon.Item{}, now))
	})

	t.Run("shows finished/total across all runs, success or failure alike", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_live", Total: 5, Done: 2, UpdatedAt: now.Add(-10 * time.Second)}, // live
			{RunID: "wf_ok1", Total: 4, Done: 4, Finalized: true, Status: "completed"},   // finished
			{RunID: "wf_ok2", Total: 3, Done: 3, Finalized: true, Status: "completed"},   // finished
			{RunID: "wf_fail", Total: 4, Done: 3, Finalized: true, Status: "failed"},     // finished (failure still counts)
		}}
		require.Contains(t, watchWorkflowCell(it, now), "3/4")
	})

	t.Run("all runs live reads 0/total", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "a", Total: 5, Done: 2, UpdatedAt: now.Add(-2 * time.Second)},
			// Balanced (every agent returned, next not launched) but not finalized,
			// so still live — not counted as finished.
			{RunID: "b", Total: 3, Done: 3, UpdatedAt: now.Add(-2 * time.Second)},
		}}
		require.Contains(t, watchWorkflowCell(it, now), "0/2")
	})

	t.Run("a crashed or finalized-failed run counts as finished, not live", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "crash", Total: 5, Done: 2, UpdatedAt: now.Add(-10 * time.Minute)}, // gone quiet
			{RunID: "fail", Total: 3, Done: 3, Finalized: true, Status: "failed"},      // finalized failure
		}}
		require.Contains(t, watchWorkflowCell(it, now), "2/2")
	})

	t.Run("a finalized run is never counted live even with a fresh mtime", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_x", Total: 5, Done: 2, Finalized: true, UpdatedAt: now},
		}}
		require.Contains(t, watchWorkflowCell(it, now), "1/1")
	})
}

func TestClassifyWorkflowRun(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-3 * time.Second)
	stale := now.Add(-10 * time.Minute)

	cases := []struct {
		name string
		run  daemon.WorkflowRun
		want workflowBucket
	}{
		{"live in-flight", daemon.WorkflowRun{Total: 5, Done: 2, UpdatedAt: fresh}, workflowLive},
		{"live but balanced", daemon.WorkflowRun{Total: 3, Done: 3, UpdatedAt: fresh}, workflowLive},
		{"empty just-started journal", daemon.WorkflowRun{Total: 0, Done: 0, UpdatedAt: fresh}, workflowLive},
		{"crashed before finalize", daemon.WorkflowRun{Total: 5, Done: 2, UpdatedAt: stale}, workflowProblem},
		{"completed", daemon.WorkflowRun{Total: 4, Done: 4, Finalized: true, Status: "completed"}, workflowDone},
		{"completed unknown status", daemon.WorkflowRun{Total: 4, Done: 4, Finalized: true}, workflowDone},
		{"finalized failure", daemon.WorkflowRun{Total: 4, Done: 4, Finalized: true, Status: "failed"}, workflowProblem},
		{"finalized with orphan", daemon.WorkflowRun{Total: 4, Done: 3, Finalized: true}, workflowProblem},
		{"finalized fresh mtime never live", daemon.WorkflowRun{Total: 5, Done: 2, Finalized: true, UpdatedAt: now}, workflowProblem},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, classifyWorkflowRun(tc.run, now), tc.name)
	}
}

func TestWatchFinishedTurn(t *testing.T) {
	m := newWatchModel(t.Context(), "", time.Second)

	working := daemon.Item{ID: "a", Activity: daemon.ActivityWorking}
	waiting := daemon.Item{ID: "a", Activity: daemon.ActivityWaiting}

	t.Run("first sight never rings", func(t *testing.T) {
		require.False(t, m.finishedTurn([]daemon.Item{working}))
	})
	t.Run("working→waiting rings once", func(t *testing.T) {
		require.True(t, m.finishedTurn([]daemon.Item{waiting}))
	})
	t.Run("staying waiting does not re-ring", func(t *testing.T) {
		require.False(t, m.finishedTurn([]daemon.Item{waiting}))
	})
	t.Run("working→working does not ring", func(t *testing.T) {
		require.False(t, m.finishedTurn([]daemon.Item{working}))
		require.False(t, m.finishedTurn([]daemon.Item{working}))
	})
	t.Run("a departed then returning container is first-seen again", func(t *testing.T) {
		require.False(t, m.finishedTurn(nil))                    // "a" leaves
		require.False(t, m.finishedTurn([]daemon.Item{waiting})) // returns idle-at-prompt, no prior
	})
	t.Run("only the transitioning container matters", func(t *testing.T) {
		b1 := daemon.Item{ID: "b", Activity: daemon.ActivityWorking}
		require.False(t, m.finishedTurn([]daemon.Item{waiting, b1}))
		b2 := daemon.Item{ID: "b", Activity: daemon.ActivityWaiting}
		require.True(t, m.finishedTurn([]daemon.Item{waiting, b2}))
	})
}

// The alias merges into the tail of the name it was derived from, so one column
// carries both. Rendering happens without a TTY, so lipgloss emits no color and
// the merged string can be compared directly.
func TestWatchName(t *testing.T) {
	cases := []struct {
		name, alias, want string
	}{
		// Segment initials: the last initial IS the last segment's first letter.
		{"lesomnus-cld", "lc", "lcld"},
		{"a-very-long-name", "avln", "avlname"},
		{"my_cool_project", "mcp", "mcproject"},
		{"acme-web-api", "awa", "awapi"},
		{"frontend-service-api", "fsa", "fsapi"},
		// A short name is its own alias, so the merge collapses to just the name.
		{"myapp", "myapp", "myapp"},
		{"ab-cd", "ab-cd", "ab-cd"},
		// A long single word is truncated, and the truncation is a prefix of it.
		{"backend", "backen", "backend"},
		// Capitals in the name must not defeat the overlap: the alias is always
		// lowercased, the name is not.
		{"Lesomnus-Cld", "lc", "lcld"},
	}
	for _, c := range cases {
		got := watchName(daemon.Item{Name: c.name, Alias: c.alias})
		require.Equalf(t, c.want, got, "watchName(%q, alias %q)", c.name, c.alias)
	}
}

// A collision-broken alias shares nothing with the name. Concatenating would
// produce a word that is neither, so the two fall back to sitting side by side.
func TestWatchNameNoOverlapFallsBack(t *testing.T) {
	require.Equal(t, "lc-3f cld", watchName(daemon.Item{Name: "lesomnus-cld", Alias: "lc-3f"}))
}

// Degenerate inputs must not panic or invent text.
func TestWatchNameDegenerate(t *testing.T) {
	require.Equal(t, "solo", watchName(daemon.Item{Name: "solo"}), "no alias yet: the name stands alone")
	require.Equal(t, "lc", watchName(daemon.Item{Alias: "lc"}), "no name yet: the alias stands alone")
	require.Equal(t, "", watchName(daemon.Item{}))
}

// Every column header must measure exactly one cell as lipgloss counts it —
// that measurement is what the padding is computed from, so a glyph the
// terminal draws wider (an Ambiguous-width symbol under a CJK locale, or an
// emoji-presentation pictograph) would shift every column after it. Guarding
// the width here catches a future glyph swap that looks fine in one terminal
// and misaligns in another.
func TestWatchHeaderGlyphWidths(t *testing.T) {
	// lipgloss must measure each single-glyph header as one cell — that is the
	// width the column padding is computed from. (The in/out/cw glyphs are
	// Ambiguous-width and may DRAW two cells under a CJK locale; that is an
	// accepted trade, so this checks only lipgloss's own measure, not runewidth's
	// wide condition.)
	for _, h := range []string{
		watchWorkflowHeader, watchWorkLastHeader,
		watchInHeader, watchOutHeader, watchCacheWriteHeader,
	} {
		require.Equalf(t, 1, lipgloss.Width(h), "header %q must be one cell wide", h)
	}
	// The qualified headers are their glyph plus an ASCII suffix.
	require.Equal(t, 2, lipgloss.Width(watchWorkTotalHeader))
	require.Equal(t, 1+len(rateUnit), lipgloss.Width(watchCacheWriteRateHeader))

	// The workflow glyph, by contrast, is deliberately Neutral-width: one cell
	// whether the terminal treats Ambiguous glyphs as narrow or wide.
	narrow := &runewidth.Condition{EastAsianWidth: false}
	wide := &runewidth.Condition{EastAsianWidth: true}
	require.Equal(t, 1, narrow.StringWidth(watchWorkflowHeader))
	require.Equal(t, 1, wide.StringWidth(watchWorkflowHeader))
}
