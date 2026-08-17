package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// The override is edited through the same path the daemon reads it from, or a
// user edits a file nothing ever loads.
func TestDindOverrideEditPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cld.yaml")

	t.Run("beside the loaded config", func(t *testing.T) {
		require.NoError(t, os.WriteFile(p, []byte("docker: {mode: dind}\n"), 0o644))
		c, err := config.ReadFromFile(p)
		require.NoError(t, err)
		require.NoError(t, c.Evaluate())

		require.Equal(t, filepath.Join(dir, config.DindFileName), c.DindOverrideEditPath())
	})

	t.Run("the file docker.compose names", func(t *testing.T) {
		require.NoError(t, os.WriteFile(p, []byte("docker: {compose: engine.yaml}\n"), 0o644))
		c, err := config.ReadFromFile(p)
		require.NoError(t, err)
		require.NoError(t, c.Evaluate())

		require.Equal(t, filepath.Join(dir, "engine.yaml"), c.DindOverrideEditPath())
	})

	t.Run("a path that does not exist yet still resolves", func(t *testing.T) {
		// Unlike the load path, which reports "" for an absent default file:
		// this is where one would be created.
		c := &config.Config{}
		require.NoError(t, c.Evaluate())
		require.Equal(t,
			filepath.Join(config.UserConfigDir(), config.DindFileName),
			c.DindOverrideEditPath())
	})
}

// A save is rejected when cld could not act on it, so a typo surfaces in the
// editor rather than as an engine that quietly never picks the setting up.
func TestDindOverrideEditValidation(t *testing.T) {
	valid := func(b []byte) error {
		_, err := config.ParseDindFile(b, "the override")
		return err
	}

	require.NoError(t, valid(dindConfigTemplate), "the seeded template must save as-is")
	require.NoError(t, valid([]byte("services:\n  dind:\n    volumes: [/a:/b]\n")))

	require.Error(t, valid([]byte("services:\n  dind:\n    healthcheck: {test: [CMD, true]}\n")))
	require.Error(t, valid([]byte("services:\n  docker:\n    cpus: 2\n")))
	require.Error(t, valid([]byte("services:\n  dind:\n    volumes: [\"~/a:/b\"]\n")))
	require.Error(t, valid([]byte("not: yaml: at all:\n")))
	require.Error(t, valid(nil), "an empty buffer has no dind service")
}
