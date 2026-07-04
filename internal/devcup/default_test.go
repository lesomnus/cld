package devcup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteDefaultConfig(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "my-project")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	p, cleanup, err := WriteDefaultConfig(ws)
	require.NoError(t, err)
	require.NotEmpty(t, p)

	b, err := os.ReadFile(p)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	require.Equal(t, "my-project", m["name"], "name is the workspace basename")
	require.NotEmpty(t, m["image"], "the built-in default carries an image")

	require.NotEqual(t, ws, filepath.Dir(p), "config is written outside the workspace")

	cleanup()
	_, err = os.Stat(p)
	require.True(t, os.IsNotExist(err), "cleanup removes the temp config")
}

func TestUpArgsWith(t *testing.T) {
	o := Options{Workspace: "/w"}

	t.Run("appends override-config", func(t *testing.T) {
		args := o.up_args_with("/x/devcontainer.json")
		require.Equal(t,
			[]string{"up", "--workspace-folder", "/w", "--override-config", "/x/devcontainer.json"},
			args)
	})
	t.Run("empty path is a no-op", func(t *testing.T) {
		require.Equal(t, o.up_args(), o.up_args_with(""))
	})
}
