package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/xli/mode"
	"github.com/lesomnus/xli/tab"
	"github.com/stretchr/testify/require"
)

// TestCompleteNamesNoDaemon pins the contract that completion never errors or
// blocks the shell: with no daemon reachable it simply emits nothing.
func TestCompleteNamesNoDaemon(t *testing.T) {
	// Point the cache dir (hence the daemon socket) at an empty directory so
	// nothing answers.
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var buf strings.Builder
	ctx := tab.Into(mode.Into(context.Background(), mode.Tab), tab.NewZshTab(&buf))
	require.NotPanics(t, func() {
		require.NoError(t, completeNames().Handle(ctx, ""))
	})
	require.Empty(t, buf.String(), "no daemon -> no candidates")
}
