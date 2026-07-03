package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionCommand(t *testing.T) {
	t.Run("no history starts a fresh session", func(t *testing.T) {
		require.Equal(t, []string{"claude"}, session_command(false))
	})
	t.Run("history resumes but falls back to a fresh session", func(t *testing.T) {
		// The fallback is the whole point: a resume Claude Code rejects
		// (`no conversation found to continue`) must not kill the pane, or
		// `cld it --new` loops forever recreating the same instant exit.
		require.Equal(t, []string{"sh", "-c", "claude --continue || exec claude"}, session_command(true))
	})
}
