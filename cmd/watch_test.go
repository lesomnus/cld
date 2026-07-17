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
