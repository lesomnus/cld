package config_test

import (
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// The example in docs/session-docker.md ("Adding a volume to the engine") is
// the first thing anyone will copy, so it is pinned here: a doc example that
// does not load is worse than no example.
func TestDocumentedVolumeExample(t *testing.T) {
	c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName, `
services:
  dind:
    volumes:
      - /srv/build-cache:/cache
`)

	got, err := c.LoadDindOverride("/work/api")
	require.NoError(t, err)
	require.Equal(t, []string{"/srv/build-cache:/cache"}, got.Volumes)

	t.Run("read-only form", func(t *testing.T) {
		c := withOverride(t, "docker: {mode: dind}\n", config.DindFileName, `
services:
  dind:
    volumes:
      - /etc/ssl/corp:/etc/docker/certs.d:ro
`)
		got, err := c.LoadDindOverride("/work/api")
		require.NoError(t, err)
		require.Equal(t, []string{"/etc/ssl/corp:/etc/docker/certs.d:ro"}, got.Volumes)
	})
}
