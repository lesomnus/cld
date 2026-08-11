package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/usage"
	"github.com/stretchr/testify/require"
)

func TestWeeklyStoreAccumulatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	s := newWeeklyStore(path)
	s.now = func() time.Time { return now }
	s.add(100, 20, 40)
	s.add(50, 10, 5)

	got := s.get()
	require.Equal(t, int64(150), got.Input)
	require.Equal(t, int64(30), got.Output)
	require.Equal(t, int64(45), got.CacheCreation)
	require.False(t, got.Empty())

	// A fresh store loading the same file recovers the totals — the whole point
	// of persistence: a daemon restart does not lose the week.
	reloaded := newWeeklyStore(path)
	reloaded.now = func() time.Time { return now }
	require.Equal(t, got, reloaded.get())
}

func TestWeeklyStoreRollsOverAtBoundary(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	reset := now.Add(48 * time.Hour)
	s := newWeeklyStore(filepath.Join(t.TempDir(), "usage.json"))
	s.now = func() time.Time { return now }

	s.anchor(reset)
	s.add(1000, 100, 200)
	require.Equal(t, int64(1000), s.get().Input)

	// Cross the weekly boundary: the window zeroes, and a new export starts the
	// next week from scratch.
	now = reset.Add(time.Minute)
	require.True(t, s.get().Empty(), "totals reset once the boundary passes")
	s.add(7, 8, 9)
	after := s.get()
	require.Equal(t, int64(7), after.Input)
	require.True(t, after.ResetsAt.IsZero(), "adopted reset is cleared until the next anchor")
}

func TestWeeklyStoreFallbackWindow(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	s := newWeeklyStore(filepath.Join(t.TempDir(), "usage.json"))
	s.now = func() time.Time { return now }

	// No anchor ever set: the window falls back to weeklyWindow after it started.
	s.add(500, 0, 0)
	require.Equal(t, int64(500), s.get().Input)

	now = now.Add(weeklyWindow + time.Hour)
	require.True(t, s.get().Empty(), "fallback window rolls over 7 days after Since")
}

func TestWeeklyReset(t *testing.T) {
	r := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	report := &UsageReport{Sources: []UsageSource{
		{Label: "broker", Usage: &usage.Usage{SevenDay: usage.Window{ResetsAt: r}}},
		{Label: "other", Usage: &usage.Usage{SevenDay: usage.Window{ResetsAt: r.Add(time.Hour)}}},
	}}
	require.Equal(t, r, weeklyReset(report), "broker-first source wins")
	require.True(t, weeklyReset(&UsageReport{}).IsZero())
	require.True(t, weeklyReset(nil).IsZero())
}

// A nil store tolerates every call, so a Daemon assembled field-by-field in a
// test never panics merely for reporting usage.
func TestWeeklyStoreNilSafe(t *testing.T) {
	var s *weeklyStore
	require.NotPanics(t, func() {
		s.add(1, 2, 3)
		s.anchor(time.Now())
		require.True(t, s.get().Empty())
	})
}
