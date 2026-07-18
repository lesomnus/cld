package cmd

import (
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/stretchr/testify/require"
)

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
	require.Contains(t, out, "WORKFLOWS")
	require.Contains(t, out, "▶1 8/12")

	noWf := base
	noWf.items = []daemon.Item{
		{Alias: "web", Name: "cld-web", Status: daemon.StatusReady, Activity: daemon.ActivityWaiting,
			ActivitySince: now.Add(-time.Minute), Title: "Fix test"},
	}
	out = noWf.table()
	require.NotContains(t, out, "WORKFLOWS", "column should collapse when no workflows")

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
