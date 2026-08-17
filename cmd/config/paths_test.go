package config_test

import (
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// The daemon runs in a container and only ever sees the host home through a
// read-only mount, so a config path has to be derived from that mount when it
// is present. Without this the daemon reads nothing and every session setting
// in cld.yaml silently does not apply.
func TestUserConfigDir(t *testing.T) {
	t.Run("through the host home mount", func(t *testing.T) {
		t.Setenv(config.HostHomeEnv, "/host-home")
		require.Equal(t, "/host-home/.config/cld", config.UserConfigDir())
	})

	t.Run("this user's own home otherwise", func(t *testing.T) {
		t.Setenv(config.HostHomeEnv, "")
		require.Equal(t, filepath.Join(home(t), ".config", "cld"), config.UserConfigDir())
	})
}

func TestDefaultConfigPaths(t *testing.T) {
	t.Setenv(config.HostHomeEnv, "/host-home")
	got := config.DefaultConfigPaths()

	// The working directory still comes first, for a checkout being worked on.
	require.Equal(t, []string{
		"cld.yaml",
		"cld.yml",
		"/host-home/.config/cld/cld.yaml",
		"/host-home/.config/cld/cld.yml",
	}, got)
}
