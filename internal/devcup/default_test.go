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

	p, err := WriteDefaultConfig(ws)
	require.NoError(t, err)

	// The config is materialized inside the workspace at the standard location,
	// so the devcontainer.config_file label points at a real, host-readable
	// file that VS Code re-reads on open.
	require.Equal(t, filepath.Join(ws, ".devcontainer", "devcontainer.json"), p)
	require.True(t, HasConfig(ws), "the workspace now has a config")

	b, err := os.ReadFile(p)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	require.Equal(t, "my-project", m["name"], "name is the workspace basename")
	require.NotEmpty(t, m["image"], "the built-in default carries an image")
}

func TestWriteDefaultConfigGitExclude(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".git"), 0o755))

	_, err := WriteDefaultConfig(ws)
	require.NoError(t, err)

	excl := filepath.Join(ws, ".git", "info", "exclude")
	b, err := os.ReadFile(excl)
	require.NoError(t, err)
	require.Contains(t, string(b), ".devcontainer/devcontainer.json",
		"the generated config is git-excluded so it does not dirty status")

	// Idempotent: a second write must not duplicate the exclude entry.
	_, err = WriteDefaultConfig(ws)
	require.NoError(t, err)
	b2, err := os.ReadFile(excl)
	require.NoError(t, err)
	require.Equal(t, string(b), string(b2), "exclude entry is not duplicated")
}
