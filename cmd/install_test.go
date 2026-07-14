package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestAuthConfigured(t *testing.T) {
	t.Run("false when nothing is configured", func(t *testing.T) {
		c := &config.Config{DataDir: t.TempDir()}
		require.False(t, auth_configured(c))
	})

	t.Run("true when a broker login exists", func(t *testing.T) {
		dir := t.TempDir()
		c := &config.Config{DataDir: dir}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "broker-credentials.json"), []byte(`{"refresh_token":"x"}`), 0o600))
		require.True(t, auth_configured(c))
	})
}
