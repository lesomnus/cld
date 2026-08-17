package cmd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Editing a file the daemon never reads is a silent failure: the setting
// simply never takes. Both ways to land there are easy to hit by accident.
func TestConfigVisibilityNote(t *testing.T) {
	const daemon_dir = "/home/me/.config/cld"

	t.Run("silent for the daemon's own config", func(t *testing.T) {
		require.Empty(t, configVisibilityNote(filepath.Join(daemon_dir, "cld.yaml"), daemon_dir, false))
		require.Empty(t, configVisibilityNote(filepath.Join(daemon_dir, "cld.dind.yaml"), daemon_dir, false))
	})

	t.Run("warns about a cld.yaml in a checkout", func(t *testing.T) {
		// The working directory is searched first, so any repo carrying a
		// cld.yaml — this one does — captures the edit.
		note := configVisibilityNote("/workspace/cld.dind.yaml", daemon_dir, false)
		require.Contains(t, note, daemon_dir)
		require.Contains(t, note, "client-only")
	})

	t.Run("warns inside a devcontainer whatever the path", func(t *testing.T) {
		// The daemon runs on the host; this filesystem is not its config, and
		// the container's own ~/.config/cld is not either.
		note := configVisibilityNote(filepath.Join(daemon_dir, "cld.yaml"), daemon_dir, true)
		require.Contains(t, note, "devcontainer")
		require.Contains(t, note, "HOST")
	})

	t.Run("silent when there is no user config dir to compare against", func(t *testing.T) {
		require.Empty(t, configVisibilityNote("/anywhere/cld.yaml", "", false))
	})
}
