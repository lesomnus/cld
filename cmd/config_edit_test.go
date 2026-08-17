package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/xli"
	"github.com/stretchr/testify/require"
)

func runConfigEdit(t *testing.T, path string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &xli.Command{}
	cmd.ErrWriter = &buf
	err := editConfigFile(t.Context(), cmd, path)
	return buf.String(), err
}

func TestValidCldConfig(t *testing.T) {
	require.NoError(t, validCldConfig([]byte("ignore:\n  - foo\n")))
	require.NoError(t, validCldConfig(cldConfigTemplate)) // comments only
	require.NoError(t, validCldConfig([]byte("")))        // empty
	require.Error(t, validCldConfig([]byte("ignore: [unclosed\n")))
}

func TestConfigEditPath(t *testing.T) {
	// No config file loaded yet: create it in the user config dir, which is the
	// only place the containerized daemon can read it from too. A cwd cld.yaml
	// would be edited happily and then never reach the daemon.
	require.Equal(t,
		filepath.Join(config.UserConfigDir(), "cld.yaml"),
		configEditPath(&config.Config{}))

	// A loaded config edits the very file it came from.
	p := filepath.Join(t.TempDir(), "custom.yaml")
	require.NoError(t, os.WriteFile(p, []byte("ignore: []\n"), 0o644))
	c, err := config.ReadFromFile(p)
	require.NoError(t, err)
	require.Equal(t, p, configEditPath(c))
}

func TestEditConfigFile(t *testing.T) {
	t.Run("writes a valid edit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cld.yaml")
		fakeEditor(t, "ignore:\n  - foo\n", 0)
		out, err := runConfigEdit(t, path)
		require.NoError(t, err)
		require.Contains(t, out, "updated")

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "ignore:\n  - foo\n", string(data))
	})

	t.Run("unchanged buffer makes no write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cld.yaml")
		// The editor writes back exactly the seeded template for a missing file.
		fakeEditor(t, string(cldConfigTemplate), 0)
		out, err := runConfigEdit(t, path)
		require.NoError(t, err)
		require.Contains(t, out, "no changes")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("a non-zero editor exit cancels", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cld.yaml")
		fakeEditor(t, "", 3)
		out, err := runConfigEdit(t, path)
		require.NoError(t, err)
		require.Contains(t, out, "cancelled")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("invalid YAML left unchanged aborts without writing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cld.yaml")
		fakeEditor(t, "ignore: [unclosed\n", 0)
		out, err := runConfigEdit(t, path)
		require.ErrorContains(t, err, "aborted")
		require.Contains(t, out, "is not valid YAML")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})
}
