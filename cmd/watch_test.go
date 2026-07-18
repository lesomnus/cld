package cmd

import (
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/stretchr/testify/require"
)

func TestWatchDuration(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	t.Run("ready reads ActivitySince and formats by magnitude", func(t *testing.T) {
		for _, tc := range []struct {
			d    time.Duration
			want string
		}{
			{5 * time.Second, "5s"},
			{90 * time.Second, "1m30s"},
			{3 * time.Minute, "3m"},
			{2*time.Hour + 5*time.Minute, "2h05m"},
			{3 * time.Hour, "3h"},
			{50 * time.Hour, "2d"},
		} {
			it := daemon.Item{Status: daemon.StatusReady, ActivitySince: ago(tc.d)}
			require.Equal(t, tc.want, watchDuration(it, now))
		}
	})

	t.Run("non-ready reads StatusSince", func(t *testing.T) {
		it := daemon.Item{Status: daemon.StatusProvisioning, StatusSince: ago(4 * time.Second)}
		require.Equal(t, "4s", watchDuration(it, now))
	})

	t.Run("a never-observed transition shows a dash, not a fabricated age", func(t *testing.T) {
		// Poll-only cross-arch container: ready but ActivitySince never stamped.
		it := daemon.Item{Status: daemon.StatusReady}
		require.Equal(t, "—", watchDuration(it, now))
	})

	t.Run("a future mark clamps to zero rather than going negative", func(t *testing.T) {
		it := daemon.Item{Status: daemon.StatusReady, ActivitySince: now.Add(time.Minute)}
		require.Equal(t, "0s", watchDuration(it, now))
	})
}

func TestWatchWorkflowCell(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	t.Run("no workflows collapses to empty", func(t *testing.T) {
		require.Equal(t, "", watchWorkflowCell(daemon.Item{}, now))
	})

	t.Run("live, aborted, and completed runs are tallied", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_live", Total: 5, Done: 2, UpdatedAt: now.Add(-10 * time.Second)},
			{RunID: "wf_ok1", Total: 4, Done: 4, Finalized: true, Status: "completed"},
			{RunID: "wf_ok2", Total: 3, Done: 3, Finalized: true, Status: "completed"},
			{RunID: "wf_abort", Total: 4, Done: 3, Finalized: true},
		}}
		cell := watchWorkflowCell(it, now)
		require.Contains(t, cell, "▶1 2/5") // one live run, 2 of 5 agents done
		require.Contains(t, cell, "⚠1")     // one aborted run
		require.Contains(t, cell, "✓2")     // two completed runs
	})

	t.Run("a finalized run is never counted live even with a fresh mtime", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_x", Total: 5, Done: 2, Finalized: true, UpdatedAt: now},
		}}
		cell := watchWorkflowCell(it, now)
		require.NotContains(t, cell, "▶")
		require.Contains(t, cell, "⚠1")
	})

	t.Run("a not-finalized run gone quiet is treated as crashed, not live", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_stale", Total: 5, Done: 2, UpdatedAt: now.Add(-10 * time.Minute)},
		}}
		cell := watchWorkflowCell(it, now)
		require.NotContains(t, cell, "▶")
		require.Contains(t, cell, "⚠1")
	})

	t.Run("a live run momentarily balanced is still live, not completed", func(t *testing.T) {
		// A sequential workflow between phases: every started agent returned,
		// the next has not launched, and no state file exists yet.
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_seq", Total: 3, Done: 3, Finalized: false, UpdatedAt: now.Add(-2 * time.Second)},
		}}
		cell := watchWorkflowCell(it, now)
		require.Contains(t, cell, "▶1 3/3")
		require.NotContains(t, cell, "✓")
	})

	t.Run("a finalized failure is a problem, not a completion", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_fail", Total: 3, Done: 3, Finalized: true, Status: "failed"},
		}}
		cell := watchWorkflowCell(it, now)
		require.Contains(t, cell, "⚠1")
		require.NotContains(t, cell, "✓")
	})

	t.Run("a finalized run with an unread status defaults to completed", func(t *testing.T) {
		it := daemon.Item{Workflows: []daemon.WorkflowRun{
			{RunID: "wf_unknown", Total: 3, Done: 3, Finalized: true, Status: ""},
		}}
		require.Contains(t, watchWorkflowCell(it, now), "✓1")
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
		require.False(t, m.finishedTurn(nil))               // "a" leaves
		require.False(t, m.finishedTurn([]daemon.Item{waiting})) // returns idle-at-prompt, no prior
	})
	t.Run("only the transitioning container matters", func(t *testing.T) {
		b1 := daemon.Item{ID: "b", Activity: daemon.ActivityWorking}
		require.False(t, m.finishedTurn([]daemon.Item{waiting, b1}))
		b2 := daemon.Item{ID: "b", Activity: daemon.ActivityWaiting}
		require.True(t, m.finishedTurn([]daemon.Item{waiting, b2}))
	})
}
