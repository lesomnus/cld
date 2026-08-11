package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/stretchr/testify/require"
)

// As the frame narrows, columns are shed in a fixed order: the workflow glyph
// first, then the in/out pair, then NAME drops to the alias alone.
func TestWatchTableShedsColumnsInOrder(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 32, 7, 0, time.UTC)
	it := daemon.Item{
		Alias: "lc", Name: "lesomnus-cld", Status: daemon.StatusReady,
		Activity: daemon.ActivityWorking, ActivitySince: now.Add(-90 * time.Second),
		Telemetry: &daemon.Telemetry{ByType: map[string]int64{
			daemon.TokenTypeInput: 1000, daemon.TokenTypeOutput: 200, daemon.TokenTypeCacheCreation: 50,
		}},
		Workflows: []daemon.WorkflowRun{{RunID: "w", Total: 3, Done: 1, UpdatedAt: now}},
	}
	render := func(w int) string {
		m := watchModel{loaded: true, now: now, width: w, items: []daemon.Item{it}}
		return stripANSI(m.table())
	}

	// Generous width: everything shows, NAME in its merged "lcld" form.
	wide := render(200)
	require.Contains(t, wide, watchWorkflowHeader)
	require.Contains(t, wide, watchInHeader)
	require.Contains(t, wide, "cld", "merged name is present at full width")

	// Scan downward, recording the widest width at which each feature is gone.
	goneWf, goneIn, shrunkName := 0, 0, 0
	for w := 200; w >= 8; w-- {
		out := render(w)
		if goneWf == 0 && !strings.Contains(out, watchWorkflowHeader) {
			goneWf = w
		}
		if goneIn == 0 && !strings.Contains(out, watchInHeader) {
			goneIn = w
		}
		if shrunkName == 0 && !strings.Contains(out, "cld") {
			shrunkName = w
		}
	}
	require.NotZero(t, goneWf)
	require.NotZero(t, goneIn)
	require.NotZero(t, shrunkName)
	require.Greater(t, goneWf, goneIn, "workflow sheds before the in/out pair")
	require.Greater(t, goneIn, shrunkName, "in/out sheds before the name shrinks to its alias")
}

// TestWatchTableWorkflowColumn checks that the WORKFLOWS column appears only
// when some container has workflow activity, and collapses otherwise.
func TestWatchTableWorkflowColumn(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 32, 7, 0, time.UTC)
	base := watchModel{loaded: true, now: now, width: 100}

	withWf := base
	withWf.items = []daemon.Item{
		{Alias: "api", Name: "cld-api", Status: daemon.StatusReady, Activity: daemon.ActivityWorking,
			ActivitySince: now.Add(-72 * time.Second), Title: "Refactor auth broker",
			Workflows: []daemon.WorkflowRun{
				{RunID: "wf_a", Total: 12, Done: 8, UpdatedAt: now.Add(-3 * time.Second)},
				{RunID: "wf_b", Total: 4, Done: 4, Finalized: true, Status: "completed"},
			}},
	}
	out := withWf.table()
	require.Contains(t, out, watchWorkflowHeader)
	require.Contains(t, out, "1/2") // wf_a live, wf_b finished → 1 of 2 finished

	noWf := base
	noWf.items = []daemon.Item{
		{Alias: "web", Name: "cld-web", Status: daemon.StatusReady, Activity: daemon.ActivityWaiting,
			ActivitySince: now.Add(-time.Minute), Title: "Fix test"},
	}
	out = noWf.table()
	require.NotContains(t, out, watchWorkflowHeader, "column should collapse when no workflows")

	// A narrow-width frame must not panic and keeps the table columns. The
	// activity column has no header and shows only a glyph, so "ACTIVITY" is
	// gone and the status word is not rendered.
	narrow := withWf
	narrow.width = 40
	out = narrow.table()
	require.Contains(t, out, "NAME")
	require.NotContains(t, out, "ACTIVITY")
	require.NotContains(t, withWf.table(), "working")
	require.Contains(t, withWf.frame_view(), watchLogo)
}

// The consumption columns appear only when some container has reported, and
// they appear together — a lone cache-write-rate column of blanks would read as
// a bug.
func TestWatchTableTelemetryColumns(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 32, 7, 0, time.UTC)
	base := watchModel{loaded: true, now: now, width: 120}

	none := base
	none.items = []daemon.Item{
		{Alias: "lc", Name: "lesomnus-cld", Status: daemon.StatusReady,
			Activity: daemon.ActivityWaiting, ActivitySince: now.Add(-time.Minute)},
	}
	out := none.table()
	require.NotContains(t, out, watchCacheWriteHeader, "columns collapse when nothing has reported")
	require.NotContains(t, out, watchCacheWriteRateHeader)

	some := base
	some.items = []daemon.Item{
		{Alias: "lc", Name: "lesomnus-cld", Status: daemon.StatusReady,
			Activity: daemon.ActivityWorking, ActivitySince: now.Add(-72 * time.Second),
			Title: "Add OTLP receiver",
			Telemetry: &daemon.Telemetry{CostUSD: 1.23, CacheCreationPerMin: 12_345,
				ByType: map[string]int64{
					daemon.TokenTypeInput:         45_600,
					daemon.TokenTypeOutput:        3_200,
					daemon.TokenTypeCacheRead:     980_000,
					daemon.TokenTypeCacheCreation: 8_900,
				}}},
		// A second container that never reported keeps its cells blank rather
		// than showing a zero.
		{Alias: "avln", Name: "a-very-long-name", Status: daemon.StatusReady,
			Activity: daemon.ActivityWaiting, ActivitySince: now.Add(-time.Minute)},
	}
	out = some.table()
	require.Contains(t, out, watchCacheWriteHeader)
	require.Contains(t, out, watchCacheWriteRateHeader)
	require.Contains(t, out, "45.6k", "input tokens")
	require.Contains(t, out, "3.2k", "output tokens")
	require.Contains(t, out, "8.9k", "cache-write tokens")
	require.NotContains(t, out, "980", "cacheRead has no column")
	// The rate cell carries the number alone; the unit is in the header.
	require.Contains(t, out, "~12.3k")
	require.NotContains(t, out, "~12.3k/m")
	require.NotContains(t, out, "$1.23", "cost stays out of the watch row")
	require.NotContains(t, out, "0.0k", "an unreported container shows no zero")
}

// The fleet-wide in/out/cw totals line renders the daemon's persisted weekly
// tally and collapses to nothing when the report is missing or the window is
// still untouched.
func TestWatchUsageTotals(t *testing.T) {
	report := &daemon.UsageReport{Weekly: daemon.WeeklyUsage{
		Input: 45_600, Output: 1_200, CacheCreation: 8_900,
	}}
	line := watchUsageTotals(report, false)
	// Glyph on the RIGHT of each value, no separators between them.
	require.Contains(t, line, "45.6k"+watchInHeader)
	require.Contains(t, line, "1.2k"+watchOutHeader)
	require.Contains(t, line, "8.9k"+watchCacheWriteHeader)
	require.NotContains(t, line, "·", "no separators between values")

	// Compact keeps only the cache-write figure.
	compact := watchUsageTotals(report, true)
	require.Contains(t, compact, "8.9k"+watchCacheWriteHeader)
	require.NotContains(t, compact, "45.6k"+watchInHeader, "input dropped when compact")
	require.NotContains(t, compact, "1.2k"+watchOutHeader, "output dropped when compact")

	require.Empty(t, watchUsageTotals(nil, false), "no report → no line")
	require.Empty(t, watchUsageTotals(&daemon.UsageReport{}, false), "untouched window → no line")
}

// The weekly totals live in the header (left of the clock), not the bottom row.
func TestWatchHeaderShowsWeeklyTotals(t *testing.T) {
	now := time.Date(2026, 8, 1, 14, 32, 7, 0, time.UTC)
	m := watchModel{loaded: true, now: now, width: 120, items: []daemon.Item{
		{Alias: "lc", Name: "lesomnus-cld", Status: daemon.StatusReady, Activity: daemon.ActivityWaiting},
	}, usage: &daemon.UsageReport{Weekly: daemon.WeeklyUsage{Input: 45_600, Output: 1_200, CacheCreation: 8_900}}}

	head := stripANSI(m.header())
	clock := m.now.Format("15:04:05")
	require.Contains(t, head, "8.9k"+watchCacheWriteHeader, "totals sit in the header")
	require.Contains(t, head, clock)
	// The totals come before the clock (they are to its left).
	require.Less(t, strings.Index(head, "8.9k"), strings.Index(head, clock))
	// And no longer in the bottom block.
	for _, line := range m.bottomBlock() {
		require.NotContains(t, stripANSI(line), "8.9k"+watchCacheWriteHeader)
	}
}

// NAME carries the alias merged in, so the separate alias column is gone.
func TestWatchTableNameMergesAlias(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 32, 7, 0, time.UTC)
	m := watchModel{loaded: true, now: now, width: 120, items: []daemon.Item{
		{Alias: "lc", Name: "lesomnus-cld", Status: daemon.StatusReady,
			Activity: daemon.ActivityWaiting, ActivitySince: now.Add(-time.Minute), Title: "t"},
	}}

	out := m.table()
	require.Contains(t, out, "lcld")
	require.NotContains(t, out, "lesomnus-cld", "the full name is replaced by the merged form")
}
