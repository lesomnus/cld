package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/xli"
	"github.com/stretchr/testify/require"
)

// fakeEditor points $EDITOR at a script that overwrites the file it is given
// with newContent, simulating a user saving that content. exitCode lets a test
// simulate a cancel (vim :cq) with a non-zero exit.
func fakeEditor(t *testing.T, newContent string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "content")
	require.NoError(t, os.WriteFile(src, []byte(newContent), 0o644))

	script := filepath.Join(dir, "editor.sh")
	body := "#!/bin/sh\ncat " + src + " > \"$1\"\n"
	if exitCode != 0 {
		body = "#!/bin/sh\nexit 3\n"
	}
	require.NoError(t, os.WriteFile(script, []byte(body), 0o755))

	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
}

func runEdit(t *testing.T, path string, target editTarget) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &xli.Command{}
	cmd.ErrWriter = &buf
	err := editUserDefault(t.Context(), cmd, path, target)
	return buf.String(), err
}

func TestResolveEditTarget(t *testing.T) {
	s, err := resolveEditTarget("")
	require.NoError(t, err)
	require.Equal(t, "settings.json", s.name)
	require.True(t, s.object)

	m, err := resolveEditTarget("claude-md")
	require.NoError(t, err)
	require.Equal(t, "CLAUDE.md", m.name)
	require.False(t, m.object)

	_, err = resolveEditTarget("nope")
	require.ErrorContains(t, err, "unknown file")
}

func TestValidSettingsObject(t *testing.T) {
	require.NoError(t, validSettingsObject([]byte(`{"model":"opus"}`)))
	require.NoError(t, validSettingsObject([]byte(`{}`)))
	require.Error(t, validSettingsObject([]byte(`[]`)))     // array, not object
	require.Error(t, validSettingsObject([]byte(`null`)))   // null
	require.Error(t, validSettingsObject([]byte(`"x"`)))    // scalar
	require.Error(t, validSettingsObject([]byte(`{bad}`)))  // not JSON
}

func TestEditUserDefault(t *testing.T) {
	settings, err := resolveEditTarget("settings")
	require.NoError(t, err)

	t.Run("writes a valid edit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		fakeEditor(t, `{"model":"opus"}`, 0)
		out, err := runEdit(t, path, settings)
		require.NoError(t, err)
		require.Contains(t, out, "updated")

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"opus"}`, string(data))
	})

	t.Run("unchanged buffer makes no write", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		// The editor writes back exactly the template the tool seeds for a
		// missing file, so nothing changed.
		fakeEditor(t, "{\n}\n", 0)
		out, err := runEdit(t, path, settings)
		require.NoError(t, err)
		require.Contains(t, out, "no changes")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("a non-zero editor exit cancels", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		fakeEditor(t, "", 3)
		out, err := runEdit(t, path, settings)
		require.NoError(t, err)
		require.Contains(t, out, "cancelled")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("invalid JSON left unchanged aborts without writing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		// The fake editor writes the same invalid content on every reopen, so the
		// tool reopens once then gives up rather than looping forever.
		fakeEditor(t, "not json", 0)
		out, err := runEdit(t, path, settings)
		require.ErrorContains(t, err, "aborted")
		require.Contains(t, out, "not a valid JSON object")
		_, statErr := os.Stat(path)
		require.True(t, os.IsNotExist(statErr))
	})

	t.Run("claude-md writes without JSON validation", func(t *testing.T) {
		md, err := resolveEditTarget("claude-md")
		require.NoError(t, err)
		path := filepath.Join(t.TempDir(), "CLAUDE.md")
		fakeEditor(t, "# Notes\nbe concise\n", 0)
		out, err := runEdit(t, path, md)
		require.NoError(t, err)
		require.Contains(t, out, "updated")

		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "# Notes\nbe concise\n", string(data))
	})
}
