package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestNeedOAuthToken(t *testing.T) {
	t.Run("true when nothing is configured", func(t *testing.T) {
		c := &config.Config{DataDir: t.TempDir()}
		require.True(t, need_oauth_token(c))
	})

	t.Run("false when auth.oauth_token_file is set", func(t *testing.T) {
		c := &config.Config{DataDir: t.TempDir()}
		c.Auth.OAuthTokenFile = "/some/path"
		require.False(t, need_oauth_token(c))
	})

	t.Run("false when a stored token already exists", func(t *testing.T) {
		dir := t.TempDir()
		c := &config.Config{DataDir: dir}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "oauth-token"), []byte("sk-ant-oat01-x"), 0o600))
		require.False(t, need_oauth_token(c))
	})
}
