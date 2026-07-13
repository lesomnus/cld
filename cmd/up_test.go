package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasConfigArg(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--remove-existing-container"}, false},
		{[]string{"--config", "x/devcontainer.json"}, true},
		{[]string{"--config=x/devcontainer.json"}, true},
		{[]string{"--override-config", "x.json"}, true},
		{[]string{"--override-config=x.json"}, true},
		{[]string{"--build-no-cache", "--config", "x"}, true},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, hasConfigArg(c.args), "args %v", c.args)
	}
}

func writeConfig(t *testing.T, ws, rel string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
}

func TestResolveConfig(t *testing.T) {
	t.Run("no config writes the built-in default", func(t *testing.T) {
		ws := t.TempDir()
		var buf bytes.Buffer
		cmd := NewCmdUp()
		cmd.ErrWriter = &buf

		extra, proceed, err := resolveConfig(cmd, ws)
		require.NoError(t, err)
		require.True(t, proceed)
		require.Nil(t, extra)
		require.FileExists(t, filepath.Join(ws, ".devcontainer", "devcontainer.json"))
		require.Contains(t, buf.String(), "wrote built-in default")
	})

	t.Run("a single standard config adds no args", func(t *testing.T) {
		ws := t.TempDir()
		writeConfig(t, ws, ".devcontainer/devcontainer.json")
		cmd := NewCmdUp()
		cmd.ErrWriter = &bytes.Buffer{}

		extra, proceed, err := resolveConfig(cmd, ws)
		require.NoError(t, err)
		require.True(t, proceed)
		require.Nil(t, extra)
	})

	t.Run("a lone sub-folder config is passed with --config", func(t *testing.T) {
		ws := t.TempDir()
		writeConfig(t, ws, ".devcontainer/go/devcontainer.json")
		cmd := NewCmdUp()
		cmd.ErrWriter = &bytes.Buffer{}

		extra, proceed, err := resolveConfig(cmd, ws)
		require.NoError(t, err)
		require.True(t, proceed)
		require.Equal(t, []string{"--config", filepath.Join(ws, ".devcontainer", "go", "devcontainer.json")}, extra)
	})

	t.Run("multiple configs without a terminal error out", func(t *testing.T) {
		ws := t.TempDir()
		writeConfig(t, ws, ".devcontainer/devcontainer.json")
		writeConfig(t, ws, ".devcontainer/go/devcontainer.json")
		cmd := NewCmdUp()
		cmd.ErrWriter = &bytes.Buffer{}

		// The test process's stdin is not a TTY, so the picker path is
		// unavailable and resolveConfig must refuse rather than guess.
		_, _, err := resolveConfig(cmd, ws)
		require.Error(t, err)
		require.Contains(t, err.Error(), "found 2 devcontainer configs")
	})
}
